package leader

// Vertex AI custom container (ADR-092 §1, PLAN T19.4). With OPOD_VERTEX=1 the
// leader also answers the two routes Vertex sends to a container it runs:
//
//   - POST $AIP_PREDICT_ROUTE: {"instances": [ … ], "parameters": { … }} →
//     {"predictions": [ … ]}. Each instance is a chat request; `parameters`
//     fill the keys an instance leaves out; each runs through the chat route
//     in parallel — admission bounds them, as it bounds any request — and its
//     prediction is the chat answer. A body without `instances` is Vertex's
//     rawPredict: a chat body passed through unchanged, streamed if it asks.
//   - GET $AIP_HEALTH_ROUTE: the leader's readiness.
//
// Vertex's IAM has authenticated the caller before a request reaches the
// container, so these routes take no key; the usage rows say protocol
// "vertex". Gemini's generateContent body ({"contents": …}) is not this shape
// and is refused by name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/api"
)

// maxVertexInstances bounds one predict request: each instance is a full chat
// request on the endpoint's workers.
const maxVertexInstances = 16

// vertexRoutes are the predict and health paths: Vertex's, else ours under /v1.
func (s *Server) vertexRoutes() (predict, health string) {
	predict, health = s.cfg.Env.VertexPredictRoute, s.cfg.Env.VertexHealthRoute
	if predict == "" {
		predict = "/v1/vertex:predict"
	}
	if health == "" {
		health = "/v1/vertex/health"
	}
	return predict, health
}

func vertexError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg, "status": http.StatusText(status)}})
}

// vertexPredict serves POST $AIP_PREDICT_ROUTE.
func (s *Server) vertexPredict(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		vertexError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if _, gemini := top["contents"]; gemini {
		vertexError(w, http.StatusBadRequest, "a Gemini generateContent body ({\"contents\": …}) is not served here: send {\"instances\": [chat requests]} to predict, or a chat body to rawPredict")
		return
	}
	ctx := api.WithProtocol(api.WithArrival(r.Context(), started), "vertex")
	rawInstances, predict := top["instances"]
	if !predict {
		// rawPredict: the body is a chat request, passed through unchanged.
		r = api.WithRequestBody(r.WithContext(ctx), body)
		s.dispatchOpenAIChat(w, r)
		return
	}
	var instances []map[string]json.RawMessage
	if err := json.Unmarshal(rawInstances, &instances); err != nil || len(instances) == 0 {
		vertexError(w, http.StatusBadRequest, "instances must be a non-empty array of chat requests")
		return
	}
	if len(instances) > maxVertexInstances {
		vertexError(w, http.StatusBadRequest, fmt.Sprintf("at most %d instances per request (got %d): each is a full chat request", maxVertexInstances, len(instances)))
		return
	}
	var params map[string]json.RawMessage
	if raw, ok := top["parameters"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			vertexError(w, http.StatusBadRequest, "parameters must be an object")
			return
		}
	}
	bodies := make([][]byte, len(instances))
	for i, inst := range instances {
		if _, ok := inst["messages"]; !ok {
			vertexError(w, http.StatusBadRequest, fmt.Sprintf("instances[%d] has no messages: each instance is a chat request", i))
			return
		}
		for k, v := range params {
			if _, set := inst[k]; !set {
				inst[k] = v
			}
		}
		if raw, ok := inst["stream"]; ok && string(raw) == "true" {
			vertexError(w, http.StatusBadRequest, fmt.Sprintf("instances[%d] asks to stream: predict answers whole predictions — use rawPredict for a stream", i))
			return
		}
		delete(inst, "stream")
		b, err := json.Marshal(inst)
		if err != nil {
			vertexError(w, http.StatusBadRequest, fmt.Sprintf("instances[%d]: %v", i, err))
			return
		}
		bodies[i] = b
	}
	answers := make([]*bufferedWriter, len(bodies))
	var wg sync.WaitGroup
	for i, b := range bodies {
		model := peekModel(b)
		if isAutoModel(model) && s.cfg.Router.DefaultModel != "" {
			model = s.cfg.Router.DefaultModel
			b = setModel(b, model)
		}
		answers[i] = &bufferedWriter{header: http.Header{}, status: http.StatusOK}
		req := api.WithRequestBody(r.WithContext(ctx), b)
		wg.Add(1)
		go func(i int, req *http.Request, model string, b []byte) {
			defer wg.Done()
			s.routeOneOpenAI(answers[i], req, model, b)
		}(i, req, model, b)
	}
	wg.Wait()
	predictions := make([]json.RawMessage, len(answers))
	for i, a := range answers {
		if a.status >= 300 {
			// One failed instance fails the request, as Vertex's own
			// containers do: a prediction list with holes would be misread.
			for k, v := range a.header {
				w.Header()[k] = v
			}
			w.Header().Set("X-Opod-Vertex-Instance", fmt.Sprint(i))
			w.WriteHeader(a.status)
			_, _ = w.Write(a.body.Bytes())
			return
		}
		predictions[i] = bytes.TrimSpace(a.body.Bytes())
	}
	writeJSON(w, http.StatusOK, map[string]any{"predictions": predictions})
}
