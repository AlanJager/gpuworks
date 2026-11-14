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

// stripClientPriority neutralizes a client's request priority: the header
// goes, and a top-level "priority" field becomes 0, which both engines read
// as the neutral level (vLLM's default, and SGLang's --default-priority-value
// 0) and which, unlike null, vLLM accepts. Bodies that do not mention the
// field are left untouched without a copy.
func stripClientPriority(r *http.Request, maxBody int64) (func(), error) {
	r.Header.Del(priorityHeader)
	named, err := bodyNames(r, priorityField)
	if err != nil || !named[0] {
		return func() {}, err
	}
	return setRequestJSONField(r, priorityField, []byte("0"), false, maxBody)
}

// bodyNames reports, in one pass over a JSON request body, which of keys it
// mentions as a quoted string anywhere (a superset of its top-level fields).
// A body that is not JSON or cannot be replayed mentions none.
func bodyNames(r *http.Request, keys ...string) ([]bool, error) {
	found := make([]bool, len(keys))
	if !isJSONRequest(r) || r.GetBody == nil {
		return found, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return found, err
	}
	defer body.Close()
	needles := make([][]byte, len(keys))
	for i, key := range keys {
		needles[i] = []byte(`"` + key + `"`)
	}
	err = readerContainsEach(body, needles, found)
	return found, err
}
