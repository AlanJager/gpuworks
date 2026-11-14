package proxy

import "net/http"

// priorityField is the request field vLLM and SGLang order their queues by,
// and priorityHeader the header vLLM 0.31 lets override it. Only the gateway
// may set a request's priority: a client's value would let any key jump the
// queue of an engine running priority scheduling.
const (
	priorityField  = "priority"
	priorityHeader = "X-Vllm-Priority"
)

// stripClientPriority neutralizes a client's request priority on every
// request: the header goes, and a top-level "priority" field becomes 0,
// which both engines read as the neutral level (vLLM's default, and SGLang's
// --default-priority-value 0) and which, unlike null, vLLM accepts. Bodies
// that do not mention the field are left untouched without a copy.
func stripClientPriority(r *http.Request, maxBody int64) (func(), error) {
	r.Header.Del(priorityHeader)
	if !isJSONRequest(r) || r.GetBody == nil {
		return func() {}, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return func() {}, err
	}
	found, err := readerContains(body, []byte(`"`+priorityField+`"`))
	_ = body.Close()
	if err != nil || !found {
		return func() {}, err
	}
	return setRequestJSONField(r, priorityField, []byte("0"), false, maxBody)
}
