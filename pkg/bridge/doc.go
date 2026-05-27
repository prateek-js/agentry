// Package bridge is the stateless mTLS/yamux routing pivot between
// the agentry CLI (on a user's laptop) and the per-cluster provisioner
// (on user-owned infrastructure).
//
// Both ends connect outbound as tunnel clients (see pkg/tunnel).
// Devices register with role=device + an X-Device-ID header; clusters
// register with role=cluster + an X-Cluster-ID header. The bridge
// holds two flat directories indexed by those IDs.
//
// When a device issues an MCP / HTTP request against a specific
// cluster, it sets X-Cluster on the request. The bridge looks up the
// cluster's session and reverse-proxies via pkg/tunnel's RoundTripper.
// The cluster session is held open by the provisioner connecting
// outbound — the bridge never dials, so the provisioner can sit
// behind a NAT with no inbound network.
//
// Identity boundary: the bridge does NOT issue certs, validate
// credentials, or hold any persistent state. The control plane
// (app.agentry.run) is the IdP + cert issuer; the bridge's role-CN
// check at /tunnel only validates that whoever made it through TLS
// is presenting a cert with a CN consistent with the role they're
// declaring. Cert issuance, organization membership, and quota
// enforcement all live one hop up the stack.
package bridge
