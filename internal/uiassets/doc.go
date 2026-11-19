// Package uiassets holds offline checks on static files the web console ships
// from ui/public, and on how ui/src draws them. It has no production code: its
// tests run in plain `go test ./...`, so a file that breaks a rule fails the
// build without a browser or a UI toolchain. See docs/dev/ui.md ("Vendor
// marks").
package uiassets
