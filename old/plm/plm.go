package plm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"time"

	"github.com/jacobsa/go-serial/serial"
)

type requestToken struct {
	*io.PipeReader
	io.Writer
	pipeWriter *io.PipeWriter
	ready      chan struct{}
}

// Close the token.
func (t *requestToken) Close() error {
	t.PipeReader.Close()
	t.pipeWriter.Close()

	return nil
}

// PowerLineModem represents an Insteon PowerLine Modem device, which can be
// connected locally or via a TCP socket.
type PowerLineModem struct {
	reader  io.Reader
	writer  io.Writer
	closer  io.Closer
	tokens  chan *requestToken
	stop    chan struct{}
	pipe    io.Closer
	aliases Aliases
	monitor Monitor
}

// ParseDevice parses a device specifiction string, either as a local file (to
// a serial port likely) or as a tcp:// URL.
func ParseDevice(device string) (io.ReadWriteCloser, error) {
	var err error
	url, _ := url.Parse(device)
	var dev io.ReadWriteCloser

	switch url.Scheme {
	case "tcp":
		dev, err = net.Dial("tcp", url.Host)

		if err != nil {
			return nil, fmt.Errorf("failed to connect to TCP device: %s", err)
		}
	case "":
		options := serial.OpenOptions{
			PortName:        url.String(),
			BaudRate:        19200,
			DataBits:        8,
			StopBits:        1,
			MinimumReadSize: 1,
		}

		// Open the port.
		dev, err = serial.Open(options)

		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported scheme for device `%s`", url.Scheme)
	}

	return dev, nil
}

// New create a new PowerLineModem device.
func New(device io.ReadWriteCloser) *PowerLineModem {
	return &PowerLineModem{
		reader:  device,
		writer:  device,
		closer:  device,
		aliases: make(aliases),
	}
}

// Aliases returns the associated aliases.
func (m *PowerLineModem) Aliases() Aliases { return m.aliases }

// SetDebugStream enables debug output on the specified writer.
func (m *PowerLineModem) SetDebugStream(w io.Writer) {
	debugStream := debugStream{
		Writer: w,
		Local:  "host",
		Remote: "plm",
	}

	m.reader = DebugReader{
		Reader:      m.reader,
		debugStream: debugStream,
	}
	m.writer = DebugWriter{
		Writer:      m.writer,
		debugStream: debugStream,
	}
}

// Start the PowerLine Modem.
//
// Attempting to start an already running intance has undefined behavior.
//
// If a responses channel is passed, it will be fed with all meaningful
// received responses and closed whenever the read ends.
func (m *PowerLineModem) Start(monitor Monitor) error {
	var readFunc func(Response)

	m.monitor = monitor

	if m.monitor != nil {
		readFunc = func(res Response) {
			m.monitor.ResponseReceived(m, res)
		}
	}

	// Create a pipe that can be connected/disconnected.
	//
	// Whenever a token becomes active, it will connect the pipe and receive
	// reads. Whenever a token closes, it will disconnect the pipe implicitely.
	pipe := &ConnectedPipe{}

	// Copy all reads to the connected pipe.
	reader := io.TeeReader(m.reader, pipe)

	m.stop = make(chan struct{})

	go readLoop(m.stop, reader, readFunc)

	m.tokens = make(chan *requestToken)
	go dispatchLoop(m.stop, m.tokens, pipe)

	m.pipe = pipe

	return nil
}

// Stop the PowerLine Modem.
//
// Attempting to stop a non-running intance has undefined behavior.
func (m *PowerLineModem) Stop() {
	close(m.stop)
	m.stop = nil
	close(m.tokens)
	m.tokens = nil

	m.pipe.Close()
}

// Close the PowerLine Modem.
func (m *PowerLineModem) Close() {
	m.Stop()
	m.closer.Close()
}

func panicStop(stop <-chan struct{}, err error) {
	select {
	case <-stop:
	default:
		panic(err)
	}
}

func readLoop(stop <-chan struct{}, r io.Reader, readFunc func(Response)) {
	responses := []Response{}

	resetResponses := func() {
		responses = []Response{
			&StandardMessageReceivedResponse{},
			&ExtendedMessageReceivedResponse{},
		}
	}

	if readFunc != nil {
		resetResponses()
	}

	for {
		i, err := UnmarshalResponses(r, responses)

		if err != nil && err != ErrCommandFailure {
			// Check if we have failure, we only panic if it wasn't expected.
			panicStop(stop, err)
		}

		if readFunc != nil {
			if err == nil {
				readFunc(responses[i])
			}

			resetResponses()
		}
	}
}

func dispatchLoop(stop <-chan struct{}, tokens <-chan *requestToken, c Connecter) {
	for token := range tokens {
		fmt.Println("dispatched one token")
		close(token.ready)
		err := c.Connect(token.pipeWriter)
		fmt.Println("token connect", err)

		// An io.ErrClosedPipe means either the Connecter or the underlying
		// Writer was closed, which are both expected.
		if err != io.ErrClosedPipe {
			// Check if we have failure, we only panic if it wasn't expected.
			panicStop(stop, err)
		}
	}
}

func (m *PowerLineModem) createToken() *requestToken {
	r, w := io.Pipe()

	token := &requestToken{
		PipeReader: r,
		Writer:     m.writer,
		pipeWriter: w,
		ready:      make(chan struct{}),
	}

	m.tokens <- token

	return token
}

// Acquire the PowerLine Modem for exclusive reading-writing.
//
// It is the responsibility of the caller to close the returned instance.
func (m *PowerLineModem) Acquire(ctx context.Context) (io.ReadWriteCloser, error) {
	token := m.createToken()
	fmt.Println("acquisition started...")

	select {
	case <-token.ready:
		fmt.Println("acquisition completed")
		return token, nil
	case <-ctx.Done():
		fmt.Println("acquisition expired")
		token.Close()
		return nil, ctx.Err()
	}
}

// GetInfo gets information about the PowerLine Modem.
func (m *PowerLineModem) GetInfo(ctx context.Context) (IMInfo, error) {
	device, err := m.Acquire(ctx)

	if err != nil {
		return IMInfo{}, err
	}

	defer device.Close()

	err = MarshalRequest(device, GetIMInfoRequest{})

	if err != nil {
		return IMInfo{}, err
	}

	var response GetIMInfoResponse

	if err := UnmarshalResponse(device, &response); err != nil {
		return IMInfo{}, err
	}

	return response.IMInfo, nil
}

func (m *PowerLineModem) sendStandardMessage(device io.ReadWriter, identity Identity, commandBytes CommandBytes) (SendStandardOrExtendedMessageResponse, error) {
	err := MarshalRequest(device, SendStandardOrExtendedMessageRequest{
		Target:       identity,
		HopsLeft:     2,
		MaxHops:      3,
		Flags:        0,
		CommandBytes: commandBytes,
	})

	if err != nil {
		return SendStandardOrExtendedMessageResponse{}, err
	}

	var response SendStandardOrExtendedMessageResponse

	if err := UnmarshalResponse(device, &response); err != nil {
		return SendStandardOrExtendedMessageResponse{}, err
	}

	return response, nil
}

func (m *PowerLineModem) sendExtendedMessage(device io.ReadWriter, identity Identity, commandBytes CommandBytes, userData UserData) (SendStandardOrExtendedMessageResponse, error) {
	err := MarshalRequest(device, SendStandardOrExtendedMessageRequest{
		Target:       identity,
		HopsLeft:     2,
		MaxHops:      3,
		Flags:        MessageFlagExtended,
		CommandBytes: commandBytes,
		UserData:     userData,
	})

	if err != nil {
		return SendStandardOrExtendedMessageResponse{}, err
	}

	var response SendStandardOrExtendedMessageResponse

	if err := UnmarshalResponse(device, &response); err != nil {
		return SendStandardOrExtendedMessageResponse{}, err
	}

	return response, nil
}

// SetLightState sets the state of a lighting device.
func (m *PowerLineModem) SetLightState(ctx context.Context, identity Identity, state LightState) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	_, err = m.sendStandardMessage(device, identity, state.commandBytes())
	return err
}

// Beep makes a device beep.
func (m *PowerLineModem) Beep(ctx context.Context, identity Identity) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	_, err = m.sendStandardMessage(device, identity, CommandBytesBeep)
	return err
}

// GetDeviceInfo gets the device info.
func (m *PowerLineModem) GetDeviceInfo(ctx context.Context, identity Identity) (DeviceInfo, error) {
	device, err := m.Acquire(ctx)

	if err != nil {
		return DeviceInfo{}, err
	}

	defer device.Close()

	_, err = m.sendExtendedMessage(device, identity, CommandBytesGetDeviceInfo, UserData{})

	// The device first sends an ack. We read it but don't really care.
	var ack StandardMessageReceivedResponse

	if err = UnmarshalResponse(device, &ack); err != nil {
		return DeviceInfo{}, err
	}

	// The device then sends information: that we care about !
	var response ExtendedMessageReceivedResponse

	if err = UnmarshalResponse(device, &response); err != nil {
		return DeviceInfo{}, err
	}

	return deviceInfoFromUserData(response.UserData), nil
}

// SetDeviceRampRate sets the ramp-rate of a device.
func (m *PowerLineModem) SetDeviceRampRate(ctx context.Context, identity Identity, rampRate time.Duration) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	userData := UserData{}
	userData[1] = 0x05
	userData[2] = rampRateToByte(rampRate)

	_, err = m.sendExtendedMessage(device, identity, CommandBytesSetDeviceInfo, userData)

	return err
}

// SetDeviceOnLevel sets the on level of a device.
func (m *PowerLineModem) SetDeviceOnLevel(ctx context.Context, identity Identity, level float64) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	userData := UserData{}
	userData[1] = 0x06
	userData[2] = onLevelToByte(level)

	_, err = m.sendExtendedMessage(device, identity, CommandBytesSetDeviceInfo, userData)

	return err
}

// SetDeviceLEDBrightness sets the LED brightness of a device.
func (m *PowerLineModem) SetDeviceLEDBrightness(ctx context.Context, identity Identity, level float64) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	userData := UserData{}
	userData[1] = 0x07
	userData[2] = ledBrightnessToByte(level)

	_, err = m.sendExtendedMessage(device, identity, CommandBytesSetDeviceInfo, userData)

	return err
}

// SetDeviceX10Address sets the X10 address of a device.
func (m *PowerLineModem) SetDeviceX10Address(ctx context.Context, identity Identity, x10HouseCode byte, x10Unit byte) error {
	device, err := m.Acquire(ctx)

	if err != nil {
		return err
	}

	defer device.Close()

	userData := UserData{}
	userData[1] = 0x04
	userData[2] = x10HouseCode
	userData[3] = x10Unit

	_, err = m.sendExtendedMessage(device, identity, CommandBytesSetDeviceInfo, userData)

	return err
}

// GetAllLinkRecords gets all the all-link records.
func (m *PowerLineModem) GetAllLinkRecords(ctx context.Context) (records AllLinkRecordList, err error) {
	device, err := m.Acquire(ctx)

	if err != nil {
		return nil, err
	}

	defer device.Close()

	err = MarshalRequest(device, GetFirstAllLinkRecordRequest{})

	if err != nil {
		return nil, err
	}

	var response GetFirstAllLinkRecordResponse

	if err = UnmarshalResponse(device, &response); err != nil {
		if err == ErrCommandFailure {
			// The database is empty. We return nil.
			return records, nil
		}

		return nil, err
	}

	var allLinkRecordResponse AllLinkRecordResponse

	for {
		if err = UnmarshalResponse(device, &allLinkRecordResponse); err != nil {
			return nil, err
		}

		records = append(records, allLinkRecordResponse.Record)

		err := MarshalRequest(device, GetNextAllLinkRecordRequest{})

		if err != nil {
			return nil, err
		}

		var nextResponse GetNextAllLinkRecordResponse

		if err = UnmarshalResponse(device, &nextResponse); err != nil {
			if err == ErrCommandFailure {
				break
			}

			return nil, err
		}
	}

	sort.Sort(records)

	return records, nil
}

// GetDeviceStatus gets the on level of a device.
func (m *PowerLineModem) GetDeviceStatus(ctx context.Context, identity Identity) (float64, error) {
	device, err := m.Acquire(ctx)

	if err != nil {
		return 0, err
	}

	defer device.Close()

	if _, err = m.sendStandardMessage(device, identity, CommandBytesStatusRequest); err != nil {
		return 0, err
	}

	var ack StandardMessageReceivedResponse

	if err = UnmarshalResponse(device, &ack); err != nil {
		return 0, err
	}

	return byteToOnLevel(ack.CommandBytes[1]), err
}