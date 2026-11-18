package services

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/skip2/go-qrcode"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
)

// bytesPerCredit is how much use one credit buys: using costs 10 credits
// per MB (10^6 bytes), so 10,000 credits are 1 GB. The price itself lives
// in the API's pricing module (api/src/pricing.ts); the API doesn't serve
// it yet, so this is the app's only copy of it.
const bytesPerCredit = 100_000

// UseOptions are UseService's seams. OpenURL opens a page in the system
// browser. SaveFile asks the member where to save a file, suggesting a
// name, and returns the path, or "" when they cancel.
type UseOptions struct {
	OpenURL  func(url string) error
	SaveFile func(suggestedName string) (string, error)
}

// UseService is Use: the member's credits, buying more through Stripe
// Checkout, where they can get a line, and the devices (WireGuard peers)
// that use one.
//
// Locking follows AuthService: no API call holds the lock.
type UseService struct {
	api     *apiclient.Client
	profile *profile.Profile
	opts    UseOptions

	// BuyCredits and AddDevice requests whose outcome is unknown, reused on
	// retry.
	buy     retained[struct{}]
	addPeer retained[newPeer]

	mu       sync.Mutex
	checkout *pendingCheckout // the checkout the member is paying, until the credits arrive
	held     *heldConfig      // the config the member is looking at, the only copy of its key
}

// newPeer is what an AddDevice retry repeats: the region, the name, and
// the device's key pair, whose public key the API may have registered.
type newPeer struct {
	country   string
	city      string
	name      string
	httpsOnly bool
	key       *ecdh.PrivateKey
}

// heldConfig is a new device's complete config, private key included. It
// lives only in memory, until the member is done with it.
type heldConfig struct {
	memberID string
	peerID   string
	fileName string
	text     []byte
}

// pendingCheckout is a checkout opened in the browser. Its credits have
// arrived once the purchased balance rises above what it was.
type pendingCheckout struct {
	memberID  string
	purchased int64
}

// NewUseService returns a UseService that calls api for the member signed
// in to p.
func NewUseService(api *apiclient.Client, p *profile.Profile, opts UseOptions) *UseService {
	return &UseService{api: api, profile: p, opts: opts}
}

// Balance is the member's credits: bought, earned (in holdback or
// cashable), their total, and how much use the total buys.
type Balance struct {
	Purchased int64 `json:"purchased"`
	Earned    int64 `json:"earned"`
	Total     int64 `json:"total"`
	// Bytes is how much data, upload plus download, Total pays for.
	Bytes int64 `json:"bytes"`
}

func balanceOf(c apiclient.CreditBalance) Balance {
	earned := c.EarnedPending + c.EarnedCashable
	total := c.Purchased + earned
	return Balance{Purchased: c.Purchased, Earned: earned, Total: total, Bytes: total * bytesPerCredit}
}

// Balance returns the member's credits.
func (s *UseService) Balance(ctx context.Context) (Balance, error) {
	sess, err := session(s.profile)
	if err != nil {
		return Balance{}, err
	}
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return Balance{}, serviceError(err)
	}
	return balanceOf(me.Credits), nil
}

// BuyCredits opens Stripe Checkout for one credit pack in the browser and
// returns the balance before the purchase. PollCheckout then tells when the
// credits arrived.
func (s *UseService) BuyCredits(ctx context.Context) (Balance, error) {
	sess, err := session(s.profile)
	if err != nil {
		return Balance{}, err
	}
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return Balance{}, serviceError(err)
	}

	// A retry after an unknown outcome gets the first checkout again
	// instead of starting a second one.
	var url string
	err = s.buy.send(sess.MemberID, nil, nil, func(requestID string, _ struct{}) (err error) {
		url, err = s.api.CreateCheckoutSession(ctx, sess.Token, requestID)
		return err
	})
	if err != nil {
		return Balance{}, serviceError(err)
	}
	if err := s.opts.OpenURL(url); err != nil {
		return Balance{}, &Error{Code: ErrUnexpected, err: err}
	}

	s.mu.Lock()
	s.checkout = &pendingCheckout{memberID: sess.MemberID, purchased: me.Credits.Purchased}
	s.mu.Unlock()
	return balanceOf(me.Credits), nil
}

// CheckoutCheck is the answer to PollCheckout.
type CheckoutCheck struct {
	// Arrived reports that the credits of the checkout BuyCredits opened
	// are in the balance. Only one poll reports it.
	Arrived bool    `json:"arrived"`
	Balance Balance `json:"balance"`
}

// PollCheckout asks the API once for the balance, and whether the credits
// of the checkout BuyCredits opened have arrived.
func (s *UseService) PollCheckout(ctx context.Context) (CheckoutCheck, error) {
	sess, err := session(s.profile)
	if err != nil {
		return CheckoutCheck{}, err
	}
	me, err := s.api.Me(ctx, sess.Token)
	if err != nil {
		return CheckoutCheck{}, serviceError(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	check := CheckoutCheck{Balance: balanceOf(me.Credits)}
	c := s.checkout
	if c == nil || c.memberID != sess.MemberID {
		return check, nil
	}
	if me.Credits.Purchased > c.purchased {
		s.checkout = nil
		check.Arrived = true
		return check, nil
	}
	// Use spends purchased credits first. Waiting from the lower balance
	// still sees the pack arrive when it is smaller than what was spent.
	c.purchased = me.Credits.Purchased
	return check, nil
}

// Region is a country with free lines for the member, and how many.
type Region struct {
	Country string `json:"country"` // ISO 3166-1 alpha-2
	Lines   int    `json:"lines"`
}

// Countries lists the countries with free lines for the member, by code.
// Never counting the member's own lines, or ones linked to them: the API
// leaves those out. With exceptPeer, a device the member moves, its line
// doesn't count either.
func (s *UseService) Countries(ctx context.Context, exceptPeer string) ([]Region, error) {
	sess, err := session(s.profile)
	if err != nil {
		return nil, err
	}
	regions, err := s.api.Regions(ctx, sess.Token, "", exceptPeer)
	if err != nil {
		return nil, serviceError(err)
	}
	out := make([]Region, 0, len(regions))
	for _, r := range regions {
		out = append(out, Region{Country: r.Country, Lines: r.Lines})
	}
	return out, nil
}

// City is a city with free lines for the member, and how many.
type City struct {
	City  string `json:"city"`
	Lines int    `json:"lines"`
}

// Cities lists the cities of country with free lines for the member, most
// lines first. Lines whose city the API doesn't know count only in
// Countries. With exceptPeer, as in Countries.
func (s *UseService) Cities(ctx context.Context, country, exceptPeer string) ([]City, error) {
	sess, err := session(s.profile)
	if err != nil {
		return nil, err
	}
	regions, err := s.api.Regions(ctx, sess.Token, country, exceptPeer)
	if err != nil {
		return nil, serviceError(err)
	}
	out := []City{}
	for _, r := range regions {
		if r.Country != country {
			continue
		}
		for _, c := range r.Cities {
			out = append(out, City{City: c.City, Lines: c.Lines})
		}
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Device is a device that uses a line: a WireGuard peer. Country, City and
// ISP say where its line is; each is empty when the API doesn't know it.
type Device struct {
	PeerID string `json:"peerId"`
	Name   string `json:"name"`
	// LineID is the line the member chose, or the API picked. The device
	// exits through it whenever it is online.
	LineID  string `json:"lineId"`
	Country string `json:"country"` // ISO 3166-1 alpha-2
	City    string `json:"city"`
	ISP     string `json:"isp"`
	// Failover is where the device goes while its line is offline:
	// FailoverCountry, FailoverCity or FailoverOff.
	Failover string `json:"failover"`
	// HTTPSOnly is whether the gateway lets through only the device's TCP
	// that opens with TLS, so the line's owner can't read or change it.
	// Every device starts with it on.
	HTTPSOnly bool `json:"httpsOnly"`
	// ExitLine is the line the device's current session exits through:
	// LineID, or a backup line while LineID is offline. Nil while the
	// device has no session.
	ExitLine  *ExitLine `json:"exitLine"`
	IPv4      string    `json:"ipv4"`
	IPv6      string    `json:"ipv6"`
	CreatedAt time.Time `json:"createdAt"`
}

// What a device does while its line is offline. Plain strings, so the
// webview can send them back as they are.
const (
	// FailoverCountry moves it to another line in the same city, else one
	// in the same country. New devices start with it.
	FailoverCountry = "country"
	// FailoverCity moves it to another line in the same city only.
	FailoverCity = "city"
	// FailoverOff leaves it without a connection until its line is back.
	FailoverOff = "off"
)

// ExitLine is the line a device's session exits through. Country (ISO
// 3166-1 alpha-2) and City are empty when the API doesn't know them.
type ExitLine struct {
	LineID  string    `json:"lineId"`
	Country string    `json:"country"`
	City    string    `json:"city"`
	Since   time.Time `json:"since"` // when the session on this line started
}

func deviceOf(p apiclient.Peer) Device {
	d := Device{
		PeerID: p.ID, Name: p.Name, LineID: p.LineID,
		Country: deref(p.Line.Country), City: deref(p.Line.City), ISP: deref(p.Line.ISP),
		Failover: p.Failover, HTTPSOnly: p.HTTPSOnly, IPv4: p.IPv4, IPv6: p.IPv6, CreatedAt: p.CreatedAt,
	}
	if e := p.ExitLine; e != nil {
		d.ExitLine = &ExitLine{LineID: e.LineID, Country: deref(e.Country), City: deref(e.City), Since: e.Since}
	}
	return d
}

// NewDevice is a device just added, with its config as a QR code for the
// WireGuard app on a phone. SaveConfig saves the same config as a .conf
// file, under FileName unless the member picks another.
type NewDevice struct {
	Device   Device `json:"device"`
	QRCode   string `json:"qrCode"` // a PNG data URL
	FileName string `json:"fileName"`
}

// AddDevice creates a WireGuard config for a device named name, on a free
// line the API picks in country, and in city unless it is empty, with
// HTTPS only on or off (see SetHTTPSOnly). The app
// makes the device's key pair and sends the API only the public key; the
// private key goes into the config, which the member sees once: it stays
// in memory until ForgetConfig, and no copy is stored.
func (s *UseService) AddDevice(ctx context.Context, country, city, name string, httpsOnly bool) (NewDevice, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxDeviceName {
		return NewDevice{}, &Error{Code: ErrInvalidName}
	}
	if country == "" {
		return NewDevice{}, &Error{Code: ErrNoCountry}
	}
	sess, err := session(s.profile)
	if err != nil {
		return NewDevice{}, err
	}

	// A retry after an unknown outcome repeats the key and request, so the
	// API replays the peer it may have created instead of making another.
	var created apiclient.PeerCreated
	var key *ecdh.PrivateKey
	err = s.addPeer.send(sess.MemberID,
		func(p newPeer) bool {
			return p.country == country && p.city == city && p.name == name && p.httpsOnly == httpsOnly
		},
		func() (newPeer, error) {
			k, err := newWireGuardKey()
			return newPeer{country: country, city: city, name: name, httpsOnly: httpsOnly, key: k}, err
		},
		func(requestID string, p newPeer) (err error) {
			key = p.key
			public := base64.StdEncoding.EncodeToString(p.key.PublicKey().Bytes())
			created, err = s.api.CreatePeer(ctx, sess.Token, requestID, country, city, name, public, httpsOnly)
			return err
		})
	if err != nil {
		return NewDevice{}, peerError(err)
	}

	if !strings.Contains(created.ConfigText, privateKeyPlaceholder) {
		return NewDevice{}, &Error{Code: ErrUnexpected, err: errors.New("the config has no private key placeholder")}
	}
	private := base64.StdEncoding.EncodeToString(key.Bytes())
	text := []byte(strings.Replace(created.ConfigText, privateKeyPlaceholder, private, 1))
	qr, err := qrcode.New(string(text), qrcode.Medium)
	if err != nil {
		return NewDevice{}, &Error{Code: ErrUnexpected, err: err}
	}
	png, err := qr.PNG(-qrModulePixels)
	if err != nil {
		return NewDevice{}, &Error{Code: ErrUnexpected, err: err}
	}

	dev := NewDevice{
		Device:   deviceOf(created.Peer),
		QRCode:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		FileName: confFileName(name),
	}
	s.mu.Lock()
	s.forgetLocked()
	s.held = &heldConfig{memberID: sess.MemberID, peerID: dev.Device.PeerID, fileName: dev.FileName, text: text}
	s.mu.Unlock()
	return dev, nil
}

const (
	// maxDeviceName is the contract's limit on a peer's name.
	maxDeviceName = 64
	// privateKeyPlaceholder is where the API's config_text wants the key.
	privateKeyPlaceholder = "{{PRIVATE_KEY}}"
	// qrModulePixels is the QR code's pixels per module: a whole number,
	// so every module stays sharp.
	qrModulePixels = 8
)

// newWireGuardKey makes a device's X25519 key pair, clamped as `wg genkey`
// writes private keys.
func newWireGuardKey() (*ecdh.PrivateKey, error) {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	b[0] &= 248
	b[31] = (b[31] & 127) | 64
	return ecdh.X25519().NewPrivateKey(b[:])
}

// confFileName suggests a .conf file name for a device. WireGuard names
// the tunnel after the file, and wg-quick on Linux takes at most 15 of
// these characters: A-Z a-z 0-9 _ = + . -
func confFileName(name string) string {
	var b strings.Builder
	b.WriteString("Homeport-")
	dash := true // after "Homeport-"
	for _, r := range name {
		ok := r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_=+.", r))
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	tunnel := b.String()
	tunnel = strings.TrimRight(tunnel[:min(len(tunnel), maxTunnelName)], "-")
	return tunnel + ".conf"
}

const maxTunnelName = 15

// peerError explains why the API refused a peer a line: a new peer, or one
// moving to another line.
func peerError(err error) error {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == "no_free_line" {
			return &Error{Code: ErrNoFreeLine, err: err}
		}
	}
	return serviceError(err)
}

// SaveConfig asks the member where to save the config of the device just
// added, and saves it there, readable only by the user. It reports false
// when the member cancels.
func (s *UseService) SaveConfig(_ context.Context, peerID string) (bool, error) {
	sess, err := session(s.profile)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	h := s.held
	s.mu.Unlock()
	if h == nil || h.peerID != peerID || h.memberID != sess.MemberID {
		return false, &Error{Code: ErrConfigGone}
	}
	path, err := s.opts.SaveFile(h.fileName)
	if err != nil {
		return false, &Error{Code: ErrUnexpected, err: err}
	}
	if path == "" {
		return false, nil
	}
	// The member may have signed out while the dialog was open, and the
	// config went with the session.
	s.mu.Lock()
	if s.held != h {
		s.mu.Unlock()
		return false, &Error{Code: ErrConfigGone}
	}
	text := slices.Clone(h.text)
	s.mu.Unlock()
	defer clear(text)
	if err := writePrivateFile(path, text); err != nil {
		return false, &Error{Code: ErrUnexpected, err: err}
	}
	return true, nil
}

// writePrivateFile writes data to path, readable only by the user, also
// when it replaces a file anyone could read.
func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ForgetConfig drops the config of the device just added, once the member
// is done with it. Its private key is gone for good.
func (s *UseService) ForgetConfig(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held != nil && s.held.peerID == peerID {
		s.forgetLocked()
	}
}

func (s *UseService) forgetLocked() {
	if s.held != nil {
		clear(s.held.text)
		s.held = nil
	}
}

// Devices lists the member's devices, newest first.
func (s *UseService) Devices(ctx context.Context) ([]Device, error) {
	sess, err := session(s.profile)
	if err != nil {
		return nil, err
	}
	peers, err := s.api.Peers(ctx, sess.Token)
	if err != nil {
		return nil, serviceError(err)
	}
	out := make([]Device, 0, len(peers))
	for _, p := range peers {
		out = append(out, deviceOf(p))
	}
	return out, nil
}

// UpdateDevice moves a device to a free line the API picks in country,
// and in city unless it is empty, or changes its failover (FailoverCountry,
// FailoverCity or FailoverOff) or HTTPS only, in one PATCH; an empty
// argument, or a nil httpsOnly, keeps what the device has. The new line is
// on the device's gateway and isn't its current one. The device keeps its
// config: within about 30 seconds the gateway ends its session on the old
// line, and its next connection exits through the new one.
func (s *UseService) UpdateDevice(ctx context.Context, peerID, country, city, failover string, httpsOnly *bool) (Device, error) {
	if (country == "" && failover == "" && httpsOnly == nil) || (city != "" && country == "") ||
		(failover != "" && failover != FailoverCountry && failover != FailoverCity && failover != FailoverOff) {
		return Device{}, &Error{Code: ErrUnexpected, err: errors.New("update a device's region, failover or HTTPS only")}
	}
	sess, err := session(s.profile)
	if err != nil {
		return Device{}, err
	}
	p, err := s.api.UpdatePeer(ctx, sess.Token, peerID, country, city, failover, httpsOnly)
	if apiclient.IsStatus(err, http.StatusNotFound) {
		return Device{}, &Error{Code: ErrDeviceGone, err: err}
	}
	if err != nil {
		return Device{}, peerError(err)
	}
	return deviceOf(p), nil
}

// SetHTTPSOnly turns HTTPS only on or off for one device, and changes
// nothing else. While it is on, the gateway lets through only the device's
// TCP that opens with TLS: plain HTTP, SSH and mail that starts
// unencrypted don't work. Off, they do, and the line's owner can read and
// change that unencrypted traffic. The change resets the device's open
// connections within about 30 seconds; its config stays.
func (s *UseService) SetHTTPSOnly(ctx context.Context, peerID string, on bool) (Device, error) {
	return s.UpdateDevice(ctx, peerID, "", "", "", &on)
}

// RemoveDevice deletes a device. Within about 30 seconds the gateway drops
// it and ends its sessions. A device that is gone already is not an error.
func (s *UseService) RemoveDevice(ctx context.Context, peerID string) error {
	sess, err := session(s.profile)
	if err != nil {
		return err
	}
	s.ForgetConfig(peerID)
	err = s.api.DeletePeer(ctx, sess.Token, peerID)
	if apiclient.IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return serviceError(err)
}

// SignOut forgets the checkout in progress and the config on screen before
// the member signs out.
//
//wails:ignore
func (s *UseService) SignOut(context.Context) {
	s.buy.reset()
	s.addPeer.reset()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkout = nil
	s.forgetLocked()
}
