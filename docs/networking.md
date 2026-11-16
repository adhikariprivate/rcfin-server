# Networking and access

RCFin can serve one public HTTP listener, an optional private listener embedded through Tailscale's `tsnet`, or both. The same application routes are used on both listeners. The public IP allow list is applied only to the public listener; the Tailscale listener skips that IP ACL. Application authentication and per-library authorization still apply on both.

```yaml
listen:
  address: "127.0.0.1" # Public listener only; useful behind a local reverse proxy.
  port: "8096"
  tls_cert: ""
  tls_key: ""
  ip_acl: "203.0.113.8,192.168.10.0/24"
  tailscale:
    enabled: true
    hostname: handler
    state_dir: /var/lib/rcfin/tsnet
    auth_key_env: RCFIN_TAILSCALE_AUTH_KEY
    control_url: "" # or https://headscale.example.com
    port: "443"
    https: true
```

## Public listener

`listen.address` and `listen.port` control the public TCP listener. An empty address binds on the default wildcard address. Set both `tls_cert` and `tls_key` to enable HTTPS. They are ignored as a TLS pair if either is empty. When running behind a reverse proxy, bind to loopback or a private interface and configure the proxy to preserve client TLS and access policy as needed.

`listen.ip_acl` is a **comma-separated allow list**, not a deny list. If empty, no client IP restriction is applied by this middleware. When non-empty, only matching source addresses are accepted; others receive HTTP 403. Entries can be individual IPv4/IPv6 addresses or CIDR ranges:

```yaml
listen:
  ip_acl: "192.168.1.25,10.20.0.0/16,2001:db8:1234::/48"
```

The check uses the connection's remote address and does not trust forwarded-IP headers such as `X-Forwarded-For`. If RCFin is behind a proxy, the ACL sees the proxy address unless the connection arrangement makes the original source address visible.

## Tailscale listener

Enable `listen.tailscale.enabled` to start RCFin as a tsnet node and accept tailnet connections. The private listener can use its own port, independently of the public listener's port and bind address. The hostname is the node name presented to the control plane. With MagicDNS enabled, tailnet clients can use the short machine name for name resolution. The full name, such as `handler.<tailnet-suffix>`, uses a suffix controlled by the tailnet.

Set `listen.tailscale.port: "443"` and `listen.tailscale.https: true` to serve HTTPS on the standard port. Clients can then open `https://handler.<tailnet-suffix>` without adding a port number. tsnet obtains a Tailscale HTTPS certificate for the full MagicDNS name. The bare short name works for MagicDNS resolution, but it is not a valid HTTPS certificate name. Tailscale HTTPS certificates must be enabled for the tailnet.

Tailscale publishes public HTTPS certificate names in Certificate Transparency logs. The device hostname and tailnet DNS name used by the certificate are visible in those logs.

Required configuration:

- `hostname`: stable node hostname, such as `handler`.
- `state_dir`: local directory where tsnet stores its identity and runtime state. RCFin creates it if needed. Persist it across restarts and deployments to reuse the same node identity.
- `auth_key_env`: name of the environment variable containing an auth key. It defaults to `RCFIN_TAILSCALE_AUTH_KEY`. When Tailscale is enabled, the variable must contain a key at startup.
- `control_url`: optional coordination server URL. Leave empty for Tailscale; set the Headscale URL for a Headscale deployment.
- `port`: private listener port. If omitted, it uses `listen.port`; if HTTPS is explicitly enabled and the port is omitted, it uses `443`.
- `https`: enables tsnet's HTTPS listener and automatic Tailscale certificate when `true`. When omitted, TLS follows the public listener for compatibility with earlier configurations.

For example, set the secret outside the YAML file:

```sh
export RCFIN_TAILSCALE_AUTH_KEY='tskey-auth-…'
```

Use a reusable or otherwise suitable unattended enrollment key according to your control server's policy. Once enrolled, persistent state is what lets tsnet reconnect as the same device; an auth key is not a replacement for preserving that state. Store the directory on a persistent volume for containers.

## Headscale

For Headscale, configure its HTTPS URL in `control_url` and provide a Headscale-compatible auth key through the configured environment variable. The key may need Headscale-specific attributes (such as a `baseURL`) depending on the Headscale key type and Tailscale client version. Follow your Headscale server's current enrollment instructions. Headscale DNS configuration determines whether the node has a useful DNS name; RCFin sets the node hostname but does not configure Headscale's DNS suffix or clients' DNS resolvers.

## Authentication and exposure boundaries

Network reachability is only the first layer. Jellyfin-compatible routes require RCFin account authentication, and collection access is checked against account grants. WebDAV also authenticates with RCFin credentials and applies the same grants. Keep the tailnet policy narrow as well: anyone permitted by the tailnet to reach the node can reach the application listener, even though application login and library authorization remain in force.

The Tailscale listener skips `listen.ip_acl` intentionally. The ACL controls only the public endpoint. This avoids requiring tailnet source addresses to be duplicated in the public allow list. A tailnet ACL/grant policy is the network-level control for the private endpoint.

## TLS note

`listen.tls_cert` and `listen.tls_key` configure HTTPS on the public listener. Setting `listen.tailscale.https: true` uses Tailscale's certificate support on the private listener and does not require copying the public certificate. Tailscale's automatic certificate flow is tied to Tailscale's control plane and DNS. For Headscale, use HTTP over the encrypted tailnet or terminate HTTPS at a proxy with a certificate valid for the name clients use. Without TLS, the tsnet listener still uses the encrypted tailnet transport; the public HTTP listener remains unencrypted.

See [configuration](configuration.md#listener-and-network-settings) for all listener fields and [accounts](accounts.md) for application-level users and grants.
