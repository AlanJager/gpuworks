package api

import "github.com/go-chi/chi/v5/middleware"

// compressJSON gzips the management API's JSON responses for clients that
// accept it. At a thousand nodes and three thousand deployments the node and
// deployment lists the pages poll run to megabytes each; JSON shrinks about
// tenfold. Only application/json is compressed: streams (server-sent events,
// logs, file and image transfers) keep their own content types and pass
// through as they are, and a websocket upgrade is handed the underlying
// connection (the writer implements http.Hijacker). The OpenAI-compatible
// gateway and the agent connection are outside the group it applies to.
var compressJSON = middleware.Compress(5, "application/json")
