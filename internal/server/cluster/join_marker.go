package cluster

import "errors"

// ErrJoinIdentityNotLive means the supplied membership does not yet contain
// this directory. The join marker is left in place.
var ErrJoinIdentityNotLive = errors.New("live membership does not contain this joining identity")

// ErrInvalidJoinMarker means the upstream join marker has unexpected content.
// Reconciliation does not delete or rewrite it.
var ErrInvalidJoinMarker = errors.New("invalid upstream join marker")
