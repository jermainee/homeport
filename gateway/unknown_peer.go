package gateway

import (
	"encoding/binary"
	"sync"
	"time"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
)

// inspectEvery is the least time between two handshake initiations the
// gateway decrypts to learn their sender. Each costs one X25519 (about
// 50 µs on a current core), so even a flood of initiations costs a few
// percent of one core.
const inspectEvery = time.Millisecond

// inspectQueue is how many initiations may wait for inspection. More are
// not inspected; a stock client sends its initiation again after 5 s.
const inspectQueue = 64

// initiation is a WireGuard handshake initiation as it arrived.
type initiation [device.MessageInitiationSize]byte

var zeroNonce [chacha20poly1305.NonceSize]byte

// unknownPeers finds handshake initiations from keys that are no peer's.
// wireguard-go drops those without telling anyone, but the sender may be a
// peer created since the gateway's last config fetch.
//
// It watches the packets wireguard-go receives (watchedBind). The packet
// path only checks an initiation's size, type and MAC1, and queues it
// without blocking. One goroutine decrypts the initiator's static key, at
// most once per inspectEvery, and calls onUnknown if no peer has it. A
// replayed initiation of a known peer, or one wireguard-go refuses for
// another reason, does not count.
type unknownPeers struct {
	priv      Key
	hash      [blake2s.Size]byte // the Noise handshake hash after mixing in the gateway's public key
	mac1      device.CookieChecker
	known     func(Key) bool
	onUnknown func()

	queue chan initiation
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newUnknownPeers(priv Key, known func(Key) bool, onUnknown func()) *unknownPeers {
	pub := priv.PublicKey()
	u := &unknownPeers{
		priv:      priv,
		hash:      mixHash(device.InitialHash[:], pub[:]),
		known:     known,
		onUnknown: onUnknown,
		queue:     make(chan initiation, inspectQueue),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	u.mac1.Init(device.NoisePublicKey(pub))
	return u
}

// offer queues pkt for inspection if it is a handshake initiation whose
// MAC1 says it was meant for this gateway. It never blocks: when the queue
// is full, pkt is not inspected.
func (u *unknownPeers) offer(pkt []byte) {
	if len(pkt) != device.MessageInitiationSize || binary.LittleEndian.Uint32(pkt) != device.MessageInitiationType {
		return
	}
	// offer runs on wireguard-go's receive goroutine, which also carries
	// every data packet. In a flood the queue is full, so skip the MAC1
	// BLAKE2s for initiations that would be dropped anyway.
	if len(u.queue) == cap(u.queue) || !u.mac1.CheckMAC1(pkt) {
		return
	}
	select {
	case u.queue <- initiation(pkt):
	default:
	}
}

// run inspects queued initiations until close.
func (u *unknownPeers) run() {
	defer close(u.done)
	for {
		select {
		case <-u.stop:
			return
		case m := <-u.queue:
			if key, ok := u.sender(&m); ok && !u.known(key) {
				u.onUnknown()
			}
			time.Sleep(inspectEvery)
		}
	}
}

// close stops run and waits for it, so onUnknown is not called afterwards.
func (u *unknownPeers) close() {
	u.once.Do(func() { close(u.stop) })
	<-u.done
}

// sender decrypts the initiator's static public key from m, the first
// steps of the responder's side of WireGuard's Noise IK handshake (as in
// wireguard-go's ConsumeMessageInitiation). ok is false unless m was
// encrypted to the gateway's key.
func (u *unknownPeers) sender(m *initiation) (key Key, ok bool) {
	ephemeral := m[8:40]
	static := m[40:88]
	hash := mixHash(u.hash[:], ephemeral)
	var chainKey, aeadKey [blake2s.Size]byte
	device.KDF1(&chainKey, device.InitialChainKey[:], ephemeral)
	ss, err := curve25519.X25519(u.priv[:], ephemeral)
	if err != nil {
		return Key{}, false // a low-order point
	}
	device.KDF2(&chainKey, &aeadKey, chainKey[:], ss)
	aead, err := chacha20poly1305.New(aeadKey[:])
	if err != nil {
		return Key{}, false
	}
	if _, err := aead.Open(key[:0], zeroNonce[:], static, hash[:]); err != nil {
		return Key{}, false
	}
	return key, true
}

// mixHash is Noise's MixHash: BLAKE2s(h || data).
func mixHash(h, data []byte) [blake2s.Size]byte {
	d, _ := blake2s.New256(nil)
	d.Write(h)
	d.Write(data)
	var sum [blake2s.Size]byte
	d.Sum(sum[:0])
	return sum
}

// watchedBind is wireguard-go's Bind, with every received packet offered
// to unknownPeers before wireguard-go sees it.
//
// As it is no *conn.StdNetBind, wireguard-go does not start its
// sticky-socket route listener on Linux. That listener clears a peer's
// cached source address when the route to the peer moves to another
// interface. The gateway does not need it: every packet a peer sends sets
// that address again, and the listener would query the route to every
// peer on each route change.
type watchedBind struct {
	conn.Bind
	unknown *unknownPeers
}

func (b *watchedBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	fns, actual, err := b.Bind.Open(port)
	if err != nil {
		return nil, 0, err
	}
	for i, recv := range fns {
		fns[i] = func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
			n, err := recv(packets, sizes, eps)
			for j := range n {
				b.unknown.offer(packets[j][:sizes[j]])
			}
			return n, err
		}
	}
	return fns, actual, nil
}
