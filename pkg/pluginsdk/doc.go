// Package pluginsdk is what a GPUWorks plugin's server part is built with.
//
// The server starts a plugin as its child and hands it two connected sockets
// (descriptors named by GWX_SERVE_FD and GWX_HOST_FD). On the first the
// plugin serves plaintext HTTP/2 to the server: the gwx/1 lifecycle calls
// (initialize, health, shutdown), then its routes and services. On the
// second it calls the server's host API, for example to push a table. The
// descriptor is the plugin's identity, so no call carries a token.
//
// A plugin's main is usually one call:
//
//	func main() {
//		pluginsdk.Main(pluginsdk.Config{
//			ID: "acme_backup", Version: "1.0.0", ManifestSHA256: manifestDigest,
//			Requires: map[string]extapi.Range{"table.banners": {Min: 1, Max: 1}},
//			Start: func(ctx context.Context, p *pluginsdk.Plugin) error {
//				_, err := p.PushState(ctx, extapi.KindBanners, &extapi.BannersV1{})
//				return err
//			},
//		})
//	}
//
// Main answers initialize (protocol and capability negotiation, refusing
// with PLUGIN_PROTOCOL_UNSUPPORTED or PLUGIN_CAPABILITY_MISSING), runs
// Start once the server accepted the handshake, answers health from
// Config.Health, and returns when the server asks it to shut down, when the
// server's end of a socket closes, or when the parent process is gone
// (polled, for hosts without a parent-death signal).
//
// PushState skips a table the server already holds with the same digest
// and numbers every push max(own counter, held generation)+1, so a plugin
// whose storage was restored or purged is never refused as stale.
//
// RequirePrincipal wraps a route's handler: it verifies the principal the
// server signed for the request under this spawn's key and refuses a
// request whose route action differs from the route the plugin matched;
// the handler reads the caller with PrincipalFrom.
//
// The package imports only the standard library and pkg/extapi. The
// plugintest package runs a plugin in process against a fake server, for
// the plugin's own tests. The protocol is described in
// docs/reference/plugin-protocol.md.
package pluginsdk
