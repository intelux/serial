package insteon

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"sync"

	"github.com/gorilla/mux"
)

// WebService implements a web-service that manages Insteon devices.
type WebService struct {
	PowerLineModem PowerLineModem
	Configuration  *Configuration

	once    sync.Once
	handler http.Handler
}

// NewWebService instanciates a new web service.
//
// If no PowerLine modem is specified, the default one is taken.
// If no configuration is specified, the default one is taken.
func NewWebService(powerLineModem PowerLineModem, configuration *Configuration) *WebService {
	return &WebService{
		PowerLineModem: powerLineModem,
		Configuration:  configuration,
	}
}

// Handler returns the HTTP handler associated to the web-service.
func (s *WebService) Handler() http.Handler {
	s.init()

	return s.handler
}

func (s *WebService) init() {
	s.once.Do(func() {
		if s.PowerLineModem == nil {
			s.PowerLineModem = DefaultPowerLineModem
		}

		if s.Configuration == nil {
			s.Configuration = &Configuration{}
		}

		s.handler = s.makeHandler()
	})
}

func (s *WebService) makeHandler() http.Handler {
	router := mux.NewRouter()

	// PLM-specific routes.
	router.Path("/plm/im-info").Methods(http.MethodGet).HandlerFunc(s.handleGetIMInfo)
	router.Path("/plm/all-link-db").Methods(http.MethodGet).HandlerFunc(s.handleGetAllLinkDB)
	router.Path("/plm/device/{id}/state").Methods(http.MethodGet).HandlerFunc(s.handleGetDeviceState)
	router.Path("/plm/device/{id}/state").Methods(http.MethodPut).HandlerFunc(s.handleSetDeviceState)
	router.Path("/plm/device/{id}/info").Methods(http.MethodGet).HandlerFunc(s.handleGetDeviceInfo)
	router.Path("/plm/device/{id}/info").Methods(http.MethodPut).HandlerFunc(s.handleSetDeviceInfo)
	router.Path("/plm/device/{id}/beep").Methods(http.MethodPost).HandlerFunc(s.handleBeep)

	// API routes.
	router.Path("/api/device/{device}/state").Methods(http.MethodGet).HandlerFunc(s.handleAPIGetDeviceState)
	router.Path("/api/device/{device}/state").Methods(http.MethodPut).HandlerFunc(s.handleAPISetDeviceState)
	router.Path("/api/device/{device}/info").Methods(http.MethodGet).HandlerFunc(s.handleAPIGetDeviceInfo)
	router.Path("/api/device/{device}/info").Methods(http.MethodPut).HandlerFunc(s.handleAPISetDeviceInfo)

	return router
}

func (s *WebService) handleGetIMInfo(w http.ResponseWriter, r *http.Request) {
	imInfo, err := s.PowerLineModem.GetIMInfo(r.Context())

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, imInfo)
}

func (s *WebService) handleGetDeviceState(w http.ResponseWriter, r *http.Request) {
	id := s.parseID(w, r)

	if id == nil {
		return
	}

	state, err := s.PowerLineModem.GetDeviceState(r.Context(), *id)

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, state)
}

func (s *WebService) handleSetDeviceState(w http.ResponseWriter, r *http.Request) {
	id := s.parseID(w, r)

	if id == nil {
		return
	}

	state := &LightState{}

	if !s.decodeValue(w, r, state) {
		return
	}

	if err := s.PowerLineModem.SetDeviceState(r.Context(), *id, *state); err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, state)
}

func (s *WebService) handleBeep(w http.ResponseWriter, r *http.Request) {
	id := s.parseID(w, r)

	if id == nil {
		return
	}

	if err := s.PowerLineModem.Beep(r.Context(), *id); err != nil {
		s.handleError(w, r, err)
		return
	}
}

func (s *WebService) handleGetDeviceInfo(w http.ResponseWriter, r *http.Request) {
	id := s.parseID(w, r)

	if id == nil {
		return
	}

	deviceInfo, err := s.PowerLineModem.GetDeviceInfo(r.Context(), *id)

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, deviceInfo)
}

func (s *WebService) handleSetDeviceInfo(w http.ResponseWriter, r *http.Request) {
	id := s.parseID(w, r)

	if id == nil {
		return
	}

	deviceInfo := &DeviceInfo{}

	if !s.decodeValue(w, r, deviceInfo) {
		return
	}

	if err := s.PowerLineModem.SetDeviceInfo(r.Context(), *id, *deviceInfo); err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, deviceInfo)
}

func (s *WebService) handleGetAllLinkDB(w http.ResponseWriter, r *http.Request) {
	records, err := s.PowerLineModem.GetAllLinkDB(r.Context())

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, records)
}

func (s *WebService) handleAPIGetDeviceState(w http.ResponseWriter, r *http.Request) {
	device := s.parseDevice(w, r)

	if device == nil {
		return
	}

	state, err := s.PowerLineModem.GetDeviceState(r.Context(), device.ID)

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, state)
}

func (s *WebService) handleAPISetDeviceState(w http.ResponseWriter, r *http.Request) {
	device := s.parseDevice(w, r)

	if device == nil {
		return
	}

	state := &LightState{}

	if !s.decodeValue(w, r, state) {
		return
	}

	if err := s.PowerLineModem.SetDeviceState(r.Context(), device.ID, *state); err != nil {
		s.handleError(w, r, err)
		return
	}

	for _, id := range device.SlaveDeviceIDs {
		s.PowerLineModem.SetDeviceState(r.Context(), id, *state)
	}

	s.handleValue(w, r, state)
}

func (s *WebService) handleAPIGetDeviceInfo(w http.ResponseWriter, r *http.Request) {
	device := s.parseDevice(w, r)

	if device == nil {
		return
	}

	deviceInfo, err := s.PowerLineModem.GetDeviceInfo(r.Context(), device.ID)

	if err != nil {
		s.handleError(w, r, err)
		return
	}

	s.handleValue(w, r, deviceInfo)
}

func (s *WebService) handleAPISetDeviceInfo(w http.ResponseWriter, r *http.Request) {
	device := s.parseDevice(w, r)

	if device == nil {
		return
	}

	deviceInfo := &DeviceInfo{}

	if !s.decodeValue(w, r, deviceInfo) {
		return
	}

	if err := s.PowerLineModem.SetDeviceInfo(r.Context(), device.ID, *deviceInfo); err != nil {
		s.handleError(w, r, err)
		return
	}

	for _, id := range device.SlaveDeviceIDs {
		s.PowerLineModem.SetDeviceInfo(r.Context(), id, *deviceInfo)
	}

	s.handleValue(w, r, deviceInfo)
}

func (s *WebService) parseID(w http.ResponseWriter, r *http.Request) *ID {
	vars := mux.Vars(r)

	idStr := vars["id"]

	if idStr == "" {
		err := fmt.Errorf("invalid empty device id")

		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s", err)

		return nil
	}

	id, err := ParseID(idStr)

	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s", err)

		return nil
	}

	return &id
}

func (s *WebService) parseDevice(w http.ResponseWriter, r *http.Request) *ConfigurationDevice {
	vars := mux.Vars(r)

	id := vars["device"]

	if id == "" {
		err := fmt.Errorf("invalid empty device identifier")

		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s", err)

		return nil
	}

	device, err := s.Configuration.LookupDevice(id)

	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "%s", err)

		return nil
	}

	return device
}

func (s *WebService) decodeValue(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	if r.Body == nil {
		err := fmt.Errorf("missing body")

		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "%s", err)

		return false
	}

	mediatype, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))

	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "%s", err)

		return false
	}

	switch mediatype {
	case "":
		mediatype = "application/json"
	case "application/json":
	default:
		err := fmt.Errorf("expected body of type application/json")

		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "%s", err)

		return false
	}

	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "%s", err)

		return false
	}

	return true
}

func (s *WebService) handleError(w http.ResponseWriter, r *http.Request, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "%s", err)
}

func (s *WebService) handleValue(w http.ResponseWriter, r *http.Request, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(value)
}