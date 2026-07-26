// Package api exposes the gateway's HTTP surface: app/proxy management,
// session lifecycle (bot + user login), and dynamic /call. Creds arrive in
// request bodies and are stored encrypted; they never come from env.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/FINWAX/tg-control-api-server/internal/secret"
	"github.com/FINWAX/tg-control-api-server/internal/session"
	"github.com/FINWAX/tg-control-api-server/internal/store"
	"github.com/FINWAX/tg-control-api-server/internal/tdjson"
)

type Server struct {
	st  *store.Store
	sec *secret.Box
	mgr *session.Manager
}

func NewServer(st *store.Store, sec *secret.Box, mgr *session.Manager) http.Handler {
	s := &Server{st: st, sec: sec, mgr: mgr}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("POST /v1/execute", s.handleExecute)
	mux.HandleFunc("POST /v1/apps", s.handleCreateApp)
	mux.HandleFunc("POST /v1/proxies", s.handleCreateProxy)
	mux.HandleFunc("POST /v1/bot", s.handleCreateBot)
	mux.HandleFunc("POST /v1/user", s.handleCreateUser)
	mux.HandleFunc("POST /v1/user/{id}/login/code", s.handleLoginCode)
	mux.HandleFunc("POST /v1/user/{id}/login/password", s.handleLoginPassword)
	mux.HandleFunc("POST /v1/user/{id}/call", s.handleCall)
	mux.HandleFunc("POST /v1/bot/{id}/call", s.handleCall)
	mux.HandleFunc("PUT /v1/user/{id}/updates/webhook", s.handleSetWebhook)
	mux.HandleFunc("PUT /v1/bot/{id}/updates/webhook", s.handleSetWebhook)
	mux.HandleFunc("DELETE /v1/user/{id}/updates/webhook", s.handleDeleteWebhook)
	mux.HandleFunc("DELETE /v1/bot/{id}/updates/webhook", s.handleDeleteWebhook)
	mux.HandleFunc("GET /v1/user/{id}", s.handleGetSession)
	mux.HandleFunc("GET /v1/bot/{id}", s.handleGetSession)
	mux.HandleFunc("GET /v1/user/{id}/files/{file_id}", s.handleDownloadFile)
	mux.HandleFunc("GET /v1/bot/{id}/files/{file_id}", s.handleDownloadFile)
	mux.HandleFunc("DELETE /v1/user/{id}", s.handleDeleteSession)
	mux.HandleFunc("DELETE /v1/bot/{id}", s.handleDeleteSession)
	mux.HandleFunc("PATCH /v1/user/{id}", s.handleUpdateSession)
	mux.HandleFunc("PATCH /v1/bot/{id}", s.handleUpdateSession)

	return mux
}

// --- envelope helpers ---

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// writeErr renders a failure that did not come from Telegram — a bad request, a
// missing session, an unreachable dependency. It is tagged source:"gateway" so a
// client can tell "our infrastructure said no" (often retryable) from "Telegram
// said no" (source:"tdlib"), without matching on the message text.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]any{"message": msg, "source": "gateway"},
	})
}

// writeCallErr renders a td_api dispatch error. A TDLib error becomes a
// structured envelope ({code, message, source:"tdlib"}) with an HTTP status
// mapped from the TDLib code; anything else falls back to a 502 gateway error.
// A FLOOD_WAIT additionally carries the wait both as error.retry_after and as
// the standard Retry-After header, so clients need not parse the message.
func writeCallErr(w http.ResponseWriter, err error) {
	var te *tdjson.Error
	if errors.As(err, &te) {
		body := map[string]any{"code": te.Code, "message": te.Message, "source": "tdlib"}
		if secs, ok := te.RetryAfter(); ok {
			body["retry_after"] = secs
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatusForTd(te.Code))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": body})
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}

// httpStatusForTd maps a TDLib error code to an HTTP status. TDLib codes largely
// mirror HTTP (400/401/403/404/…); 420 is FLOOD_WAIT (rate limit).
func httpStatusForTd(code int) int {
	switch {
	case code == 0:
		return http.StatusBadGateway // unparseable / no code
	case code == 420:
		return http.StatusTooManyRequests
	case code >= 400 && code <= 599:
		return code
	default:
		return http.StatusBadRequest
	}
}

// maxBodyBytes caps a request body so an oversized payload can't exhaust memory
// during JSON decode. Generous enough for any td_api /call params (including
// inline file data), while still bounding worst case.
const maxBodyBytes = 8 << 20 // 8 MiB

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// --- synchronous dynamic dispatch (no auth) ---

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Method == "" {
		writeErr(w, http.StatusBadRequest, "method is required")
		return
	}
	if session.IsBlockedMethod(req.Method) {
		writeErr(w, http.StatusForbidden, "method reserved by the gateway")
		return
	}
	result, err := tdjson.ExecuteSync(req.Method, req.Params)
	if err != nil {
		writeCallErr(w, err)
		return
	}
	writeOK(w, result)
}

// --- management ---

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIID   int32  `json:"api_id"`
		APIHash string `json:"api_hash"`
		Label   string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.APIID == 0 || req.APIHash == "" {
		writeErr(w, http.StatusBadRequest, "api_id and api_hash are required")
		return
	}
	enc, err := s.sec.Encrypt([]byte(req.APIHash))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := s.st.CreateApp(r.Context(), req.APIID, enc, req.Label)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"app_id": id})
}

func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type     string `json:"type"`
		Host     string `json:"host"`
		Port     int32  `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Secret   string `json:"secret"`
		Label    string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Type == "" || req.Host == "" || req.Port == 0 {
		writeErr(w, http.StatusBadRequest, "type, host, port are required")
		return
	}
	plain := req.Password
	if req.Type == "mtproto" {
		plain = req.Secret
	}
	var enc []byte
	if plain != "" {
		var err error
		if enc, err = s.sec.Encrypt([]byte(plain)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	id, err := s.st.CreateProxy(r.Context(), store.Proxy{
		Type: req.Type, Host: req.Host, Port: req.Port,
		Username: req.Username, SecretEnc: enc, Label: req.Label,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"proxy_id": id})
}

// --- sessions ---

func (s *Server) handleCreateBot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token   string `json:"token"`
		AppID   string `json:"app_id"`
		ProxyID string `json:"proxy_id"`
		Label   string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Token == "" || req.AppID == "" {
		writeErr(w, http.StatusBadRequest, "token and app_id are required")
		return
	}
	id, me, err := s.mgr.CreateBot(r.Context(), req.AppID, req.Token, req.ProxyID, req.Label)
	if err != nil {
		writeCallErr(w, err)
		return
	}
	writeOK(w, map[string]any{"id": id, "status": "authorized", "me": json.RawMessage(me)})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID   string `json:"app_id"`
		Phone   string `json:"phone"`
		ProxyID string `json:"proxy_id"`
		Label   string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.AppID == "" || req.Phone == "" {
		writeErr(w, http.StatusBadRequest, "app_id and phone are required")
		return
	}
	id, status, err := s.mgr.CreateUser(r.Context(), req.AppID, req.Phone, req.ProxyID, req.Label)
	if err != nil {
		writeCallErr(w, err)
		return
	}
	writeOK(w, map[string]any{"id": id, "status": status})
}

func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	status, err := s.mgr.SubmitCode(r.PathValue("id"), req.Code)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": status})
}

func (s *Server) handleLoginPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	status, err := s.mgr.SubmitPassword(r.PathValue("id"), req.Password)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": status})
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Method == "" {
		writeErr(w, http.StatusBadRequest, "method is required")
		return
	}
	if session.IsBlockedMethod(req.Method) {
		writeErr(w, http.StatusForbidden, "method reserved by the gateway")
		return
	}
	result, err := s.mgr.Call(r.Context(), r.PathValue("id"), req.Method, req.Params)
	if err != nil {
		if errors.Is(err, session.ErrLocalPathDenied) {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		writeCallErr(w, err)
		return
	}
	writeOK(w, json.RawMessage(result))
}

func (s *Server) handleSetWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string `json:"url"`
		Secret  string `json:"secret"`
		Filters struct {
			Types []string `json:"types"`
		} `json:"filters"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.URL == "" {
		writeErr(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := s.mgr.SetWebhook(r.Context(), r.PathValue("id"), req.URL, req.Secret, req.Filters.Types); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": "ok"})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteWebhook(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": "deleted"})
}

func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label   *string `json:"label"`
		ProxyID *string `json:"proxy_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.mgr.UpdateSession(r.Context(), r.PathValue("id"), req.Label, req.ProxyID); err != nil {
		if err.Error() == "session not found" {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteSession(r.Context(), r.PathValue("id")); err != nil {
		if err.Error() == "session not found" {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"status": "deleted"})
}

// handleDownloadFile downloads a file (by numeric file_id in the path, or by a
// persistent ?remote_id=) onto this worker and streams it back with Range
// support. All td_api work happens before any bytes are written, so an error can
// still be reported as a JSON envelope; only once the file is open do we stream.
// ?delete=1 drops the file from TDLib storage after a full (non-Range) download.
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, fileID, err := s.mgr.DownloadFile(r.Context(), id, r.PathValue("file_id"), r.URL.Query().Get("remote_id"))
	if err != nil {
		switch {
		case errors.Is(err, session.ErrBadFileRef):
			writeErr(w, http.StatusBadRequest, err.Error())
		case err.Error() == "session not found":
			writeErr(w, http.StatusNotFound, err.Error())
		default:
			writeCallErr(w, err) // td_api error -> structured; otherwise 502
		}
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "downloaded file unavailable")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stat downloaded file")
		return
	}
	name := filepath.Base(path)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	isRange := r.Header.Get("Range") != ""
	http.ServeContent(w, r, name, fi.ModTime(), f) // handles Content-Type/Length + Range
	// Only prune on a full download; a partial (Range) response is likely a resume.
	if r.URL.Query().Get("delete") == "1" && !isRange {
		s.mgr.DeleteTdFile(r.Context(), id, fileID)
	}
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, err := s.mgr.Status(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id, "status": status})
}
