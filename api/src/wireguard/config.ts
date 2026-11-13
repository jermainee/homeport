/**
 * WireGuard tunnel settings every peer's .conf file gets. Each peer has one
 * host number, used in both tunnel networks: 10.77.0.0/16 and fd77::/64.
 * Host 1 is the gateway (10.77.0.1), which also answers DNS.
 */
export const FIRST_PEER_HOST = 2;
export const LAST_PEER_HOST = 65534;
export const TUNNEL_DNS = "10.77.0.1";
/** Small enough for a tunnel inside common travel-router and mobile uplinks. */
export const TUNNEL_MTU = 1280;
export const PERSISTENT_KEEPALIVE_SECONDS = 25;

/** The peer's IPv4 tunnel address for a host number: 10.77.a.b with a × 256 + b = host. */
export function tunnelIpv4(host: number): string {
  return `10.77.${Math.floor(host / 256)}.${host % 256}`;
}

/** The peer's IPv6 tunnel address for a host number: fd77::<host in hex>. */
export function tunnelIpv6(host: number): string {
  return `fd77::${host.toString(16)}`;
}

/**
 * The complete .conf file, except for the private key: the client puts its
 * own key where `{{PRIVATE_KEY}}` stands, so the API never sees it. The
 * whole tunnel goes through the gateway (AllowedIPs 0.0.0.0/0, ::/0), DNS
 * included, so the home line's resolver answers.
 */
export function wireguardConfig(peer: { host: number }, gateway: { publicKey: string; endpoint: string }): string {
  return [
    "[Interface]",
    "PrivateKey = {{PRIVATE_KEY}}",
    `Address = ${tunnelIpv4(peer.host)}/32, ${tunnelIpv6(peer.host)}/128`,
    `DNS = ${TUNNEL_DNS}`,
    `MTU = ${TUNNEL_MTU}`,
    "",
    "[Peer]",
    `PublicKey = ${gateway.publicKey}`,
    "AllowedIPs = 0.0.0.0/0, ::/0",
    `Endpoint = ${gateway.endpoint}`,
    `PersistentKeepalive = ${PERSISTENT_KEEPALIVE_SECONDS}`,
    "",
  ].join("\n");
}
