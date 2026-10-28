package proxy

// ClientIPHeader carries the address of the client, as Caddy sees it, to the
// Belune API. Caddy SETs it on every route that reaches the dashboard (a set
// replaces whatever the client sent), from {http.vars.client_ip}, which is the
// real visitor even behind a CDN that is listed in trusted_proxies.
//
// ⚠️ The header alone authenticates nothing: anything that can open a connection
// to the API can send it. The API honours it only when the TCP peer is the Caddy
// container (middleware.ProxyTrust), never because it is present.
const ClientIPHeader = "X-Belune-Client-IP"
