package services_test

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"github.com/jermainee/homeport/desktop-app/internal/apiclient"
	"github.com/jermainee/homeport/desktop-app/internal/fakeapi"
	"github.com/jermainee/homeport/desktop-app/internal/profile"
	"github.com/jermainee/homeport/desktop-app/services"
)

// useFixture is a verified member with a UseService on their profile.
type useFixture struct {
	api     *fakeapi.Server
	dir     string
	browser *browser
	saver   *saver
	use     *services.UseService
}

func newUse(t *testing.T) *useFixture {
	t.Helper()
	api := fakeapi.New(t)
	dir := t.TempDir()
	signIn(t, api, newAuth(t, api, dir), email)
	api.Verify(t, email)
	p, err := profile.Open(dir)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	f := &useFixture{api: api, dir: dir, browser: &browser{}, saver: &saver{}}
	f.use = services.NewUseService(apiclient.New(api.URL, api.Client()), p, services.UseOptions{
		OpenURL:  f.browser.Open,
		SaveFile: f.saver.Choose,
	})
	return f
}

func TestBalanceShowsBoughtAndEarnedCredits(t *testing.T) {
	f := newUse(t)
	f.api.SetCredits(t, email, fakeapi.Credits{Purchased: 25_000, EarnedPending: 4_000, EarnedCashable: 1_000})

	got, err := f.use.Balance(t.Context())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}

	want := services.Balance{Purchased: 25_000, Earned: 5_000, Total: 30_000, Bytes: 3_000_000_000}
	if got != want {
		t.Errorf("balance = %+v, want %+v", got, want)
	}
}

// 10 credits buy 1 MB (10^6 bytes), so 10,000 credits are 1 GB.
func TestBalanceInBytes(t *testing.T) {
	f := newUse(t)

	for credits, bytes := range map[int64]int64{0: 0, 10: 1_000_000, 10_000: 1_000_000_000, 1_000_000: 100_000_000_000} {
		f.api.SetCredits(t, email, fakeapi.Credits{Purchased: credits})
		got, err := f.use.Balance(t.Context())
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		if got.Bytes != bytes {
			t.Errorf("%d credits = %d bytes, want %d", credits, got.Bytes, bytes)
		}
	}
}

func TestBuyCreditsOpensCheckoutInTheBrowser(t *testing.T) {
	f := newUse(t)

	if _, err := f.use.BuyCredits(t.Context()); err != nil {
		t.Fatalf("BuyCredits: %v", err)
	}

	sessions := f.api.CheckoutSessions(t, email)
	if len(sessions) != 1 {
		t.Fatalf("checkouts started = %d, want 1", len(sessions))
	}
	if urls := f.browser.URLs(); !slices.Equal(urls, sessions) {
		t.Errorf("browser opened %q, want the checkout %q", urls, sessions)
	}
}

func TestBuyCreditsWaitsUntilTheCreditsArrive(t *testing.T) {
	f := newUse(t)
	f.api.SetCredits(t, email, fakeapi.Credits{Purchased: 500, EarnedCashable: 200})
	if _, err := f.use.BuyCredits(t.Context()); err != nil {
		t.Fatalf("BuyCredits: %v", err)
	}

	// The member is still paying.
	got, err := f.use.PollCheckout(t.Context())
	if err != nil {
		t.Fatalf("PollCheckout: %v", err)
	}
	if got.Arrived {
		t.Fatalf("credits arrived before the payment: %+v", got)
	}

	// Stripe's webhook grants the pack.
	f.api.GrantPurchased(t, email, 1_000_000)
	got, err = f.use.PollCheckout(t.Context())
	if err != nil {
		t.Fatalf("PollCheckout: %v", err)
	}
	want := services.Balance{Purchased: 1_000_500, Earned: 200, Total: 1_000_700, Bytes: 100_070_000_000}
	if !got.Arrived || got.Balance != want {
		t.Errorf("after payment: %+v, want arrived with %+v", got, want)
	}

	// The checkout is done, so a later poll has nothing to wait for.
	got, err = f.use.PollCheckout(t.Context())
	if err != nil {
		t.Fatalf("PollCheckout: %v", err)
	}
	if got.Arrived {
		t.Errorf("a second poll says credits arrived again: %+v", got)
	}
}

// Use that goes on while the member pays spends purchased credits, so the
// balance can fall below what it was before the purchase.
func TestBuyCreditsIsNotFooledBySpending(t *testing.T) {
	f := newUse(t)
	f.api.SetCredits(t, email, fakeapi.Credits{Purchased: 5_000})
	if _, err := f.use.BuyCredits(t.Context()); err != nil {
		t.Fatalf("BuyCredits: %v", err)
	}
	f.api.SetCredits(t, email, fakeapi.Credits{Purchased: 4_000})

	got, err := f.use.PollCheckout(t.Context())
	if err != nil {
		t.Fatalf("PollCheckout: %v", err)
	}
	if got.Arrived {
		t.Errorf("credits arrived although only spending happened: %+v", got)
	}
	if got.Balance.Total != 4_000 {
		t.Errorf("balance = %+v, want the spent-down 4,000", got.Balance)
	}
}

func TestBuyCreditsNeedsAVerifiedIdentity(t *testing.T) {
	f := newUse(t)
	f.api.SetIdentityStatus(t, email, "requires_input")

	_, err := f.use.BuyCredits(t.Context())

	if code := errCode(err); code != services.ErrIdentityRequired {
		t.Errorf("error = %v, want code %q", err, services.ErrIdentityRequired)
	}
	if urls := f.browser.URLs(); len(urls) != 0 {
		t.Errorf("browser opened %q, want nothing", urls)
	}
}

func TestBuyCreditsRetriesWithTheSameRequest(t *testing.T) {
	f := newUse(t)
	f.api.LoseNextResponse("POST /v1/checkout/sessions")

	if _, err := f.use.BuyCredits(t.Context()); err == nil {
		t.Fatal("BuyCredits succeeded although the answer was lost")
	}
	if _, err := f.use.BuyCredits(t.Context()); err != nil {
		t.Fatalf("BuyCredits again: %v", err)
	}

	if sessions := f.api.CheckoutSessions(t, email); len(sessions) != 1 {
		t.Errorf("checkouts started = %d, want 1", len(sessions))
	}
}

func TestBuyCreditsStartsAnewWhenTheRetryIsTooOld(t *testing.T) {
	f := newUse(t)
	f.api.LoseNextResponse("POST /v1/checkout/sessions")
	if _, err := f.use.BuyCredits(t.Context()); err == nil {
		t.Fatal("BuyCredits succeeded although the answer was lost")
	}
	f.api.FailNext("POST /v1/checkout/sessions", http.StatusBadRequest, "request_id_expired")

	if _, err := f.use.BuyCredits(t.Context()); err != nil {
		t.Fatalf("BuyCredits a day later: %v", err)
	}

	if sessions := f.api.CheckoutSessions(t, email); len(sessions) != 2 {
		t.Errorf("checkouts started = %d, want the lost one and a new one", len(sessions))
	}
}

func TestBuyCreditsWhileTooManyCheckoutsSaysWait(t *testing.T) {
	f := newUse(t)
	f.api.FailNext("POST /v1/checkout/sessions", http.StatusTooManyRequests, "rate_limited")

	_, err := f.use.BuyCredits(t.Context())

	if code := errCode(err); code != services.ErrRateLimited {
		t.Errorf("error = %v, want code %q", err, services.ErrRateLimited)
	}
}

// saver stands in for the save dialog: it answers with path, or with ""
// when the member cancels, and records the names it suggested.
type saver struct {
	mu        sync.Mutex
	path      string
	suggested []string
	during    func() // runs while the dialog is open, if set
}

func (s *saver) Choose(suggested string) (string, error) {
	s.mu.Lock()
	s.suggested = append(s.suggested, suggested)
	path, during := s.path, s.during
	s.mu.Unlock()
	if during != nil {
		during()
	}
	return path, nil
}

func TestCountriesSaysWhereFreeLinesAre(t *testing.T) {
	f := newUse(t)
	f.offer("DE", "Berlin", "Deutsche Telekom")
	f.offer("DE", "Munich", "")
	f.offer("PT", "", "")
	f.offer("", "", "") // where the API doesn't know: never listed

	got, err := f.use.Countries(t.Context())
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}

	want := []services.Region{{Country: "DE", Lines: 2}, {Country: "PT", Lines: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("countries = %+v, want %+v", got, want)
	}
}

// The webview gets [], not null, when nobody shares a line.
func TestCountriesWhenNoneAreSharedIsEmpty(t *testing.T) {
	f := newUse(t)

	got, err := f.use.Countries(t.Context())
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("countries = %#v, want an empty list", got)
	}
}

func TestCountriesNeedAVerifiedIdentity(t *testing.T) {
	f := newUse(t)
	f.api.SetIdentityStatus(t, email, "processing")

	_, err := f.use.Countries(t.Context())

	if code := errCode(err); code != services.ErrIdentityRequired {
		t.Errorf("error = %v, want code %q", err, services.ErrIdentityRequired)
	}
}

func TestCitiesListsACountrysCitiesMostLinesFirst(t *testing.T) {
	f := newUse(t)
	f.offer("DE", "Berlin", "")
	f.offer("DE", "Munich", "")
	f.offer("DE", "Munich", "")
	f.offer("DE", "", "")
	f.offer("PT", "Porto", "")

	got, err := f.use.Cities(t.Context(), "DE")
	if err != nil {
		t.Fatalf("Cities: %v", err)
	}

	want := []services.City{{City: "Munich", Lines: 2}, {City: "Berlin", Lines: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("cities = %+v, want %+v", got, want)
	}
}

// The webview gets [], not null, for a country without free lines.
func TestCitiesOfACountryWithoutLinesIsEmpty(t *testing.T) {
	f := newUse(t)

	got, err := f.use.Cities(t.Context(), "DE")
	if err != nil {
		t.Fatalf("Cities: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("cities = %#v, want an empty list", got)
	}
}

// offer offers an online line in country and city (both may be empty) with
// isp, and returns its id.
func (f *useFixture) offer(country, city, isp string) string {
	line := fakeapi.AvailableLine{Country: country, Online: true}
	if city != "" {
		line.City = &city
	}
	if isp != "" {
		line.ISP = &isp
	}
	return f.api.OfferLine(line)
}

// offerBerlin offers a line in Berlin and returns its id.
func (f *useFixture) offerBerlin() string {
	return f.offer("DE", "Berlin", "")
}

// offerBerlinCity offers a line in Berlin and returns the city, for addDevice.
func (f *useFixture) offerBerlinCity() string {
	f.offerBerlin()
	return "Berlin"
}

// addDevice adds a device in Germany, in city unless it is empty, and
// returns it with the config its QR code holds.
func (f *useFixture) addDevice(t *testing.T, city, name string) (services.NewDevice, string) {
	t.Helper()
	got, err := f.use.AddDevice(t.Context(), "DE", city, name)
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	return got, decodeQR(t, got.QRCode)
}

// decodeQR reads the text of a QR code given as a PNG data URL.
func decodeQR(t *testing.T, dataURL string) string {
	t.Helper()
	b64, ok := strings.CutPrefix(dataURL, "data:image/png;base64,")
	if !ok {
		t.Fatalf("QR code is not a PNG data URL: %.40q", dataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("QR code base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("QR code PNG: %v", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatalf("QR code bitmap: %v", err)
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		t.Fatalf("QR code doesn't scan: %v", err)
	}
	return res.GetText()
}

// privateKey returns the PrivateKey line's key from a WireGuard config.
func privateKey(t *testing.T, config string) string {
	t.Helper()
	for line := range strings.Lines(config) {
		if k, ok := strings.CutPrefix(strings.TrimSpace(line), "PrivateKey = "); ok {
			return k
		}
	}
	t.Fatalf("config has no PrivateKey:\n%s", config)
	return ""
}

// publicKeyOf derives the WireGuard public key of a private key.
func publicKeyOf(t *testing.T, private string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(private)
	if err != nil {
		t.Fatalf("private key %q is not base64: %v", private, err)
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

func TestAddDeviceGetsAFreeLineInTheCity(t *testing.T) {
	f := newUse(t)
	f.offer("DE", "Berlin", "")
	munich := f.offer("DE", "Munich", "M-net")

	got, config := f.addDevice(t, "Munich", "iPhone")

	peers := f.api.Peers(t, email)
	if len(peers) != 1 {
		t.Fatalf("peers = %d, want 1", len(peers))
	}
	p := peers[0]
	if p.Name != "iPhone" || p.LineID != munich {
		t.Errorf("peer = %q on %s, want iPhone on %s", p.Name, p.LineID, munich)
	}
	want := services.Device{
		PeerID: p.PeerID, Name: "iPhone", LineID: munich, Country: "DE", City: "Munich", ISP: "M-net",
		Failover: services.FailoverCountry, IPv4: p.IPv4, IPv6: p.IPv6, CreatedAt: p.CreatedAt,
	}
	if got.Device != want {
		t.Errorf("device = %+v, want %+v", got.Device, want)
	}
	for _, line := range []string{
		"Address = " + p.IPv4 + "/32, " + p.IPv6 + "/128",
		"PublicKey = " + fakeapi.GatewayWGPublicKey,
		"Endpoint = " + fakeapi.GatewayWGEndpoint,
	} {
		if !strings.Contains(config, line+"\n") {
			t.Errorf("config lacks %q:\n%s", line, config)
		}
	}
}

func TestAddDeviceInAnyCityOfTheCountry(t *testing.T) {
	f := newUse(t)
	f.offer("PT", "Porto", "")
	berlin := f.offerBerlin()

	got, _ := f.addDevice(t, "", "Laptop")

	if got.Device.LineID != berlin || got.Device.Country != "DE" {
		t.Errorf("device on %s in %q, want %s in DE", got.Device.LineID, got.Device.Country, berlin)
	}
}

func TestAddDeviceWhenTheRegionHasNoFreeLine(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()

	_, err := f.use.AddDevice(t.Context(), "DE", "Hamburg", "iPhone")

	if code := errCode(err); code != services.ErrNoFreeLine {
		t.Errorf("error = %v, want code %q", err, services.ErrNoFreeLine)
	}
}

func TestAddDeviceNeedsACountry(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()

	_, err := f.use.AddDevice(t.Context(), "", "", "iPhone")

	if code := errCode(err); code != services.ErrNoCountry {
		t.Errorf("error = %v, want code %q", err, services.ErrNoCountry)
	}
	if peers := f.api.Peers(t, email); len(peers) != 0 {
		t.Errorf("peers = %d, want none", len(peers))
	}
}

// The app makes the device's key pair and sends the API only the public
// key; the private key goes into the config, and nowhere else.
func TestAddDeviceKeepsThePrivateKeyOutOfTheAPIAndTheProfile(t *testing.T) {
	f := newUse(t)

	f.offerBerlin()
	_, config := f.addDevice(t, "Berlin", "iPhone")

	if strings.Contains(config, "{{PRIVATE_KEY}}") {
		t.Fatalf("config still has the placeholder:\n%s", config)
	}
	private := privateKey(t, config)
	if got, want := publicKeyOf(t, private), f.api.Peers(t, email)[0].PublicKey; got != want {
		t.Errorf("config's key belongs to public key %s, but the API has %s", got, want)
	}
	err := filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(private)) {
			t.Errorf("the profile stores the private key in %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddDeviceRetriesWithTheSameKeyAndRequest(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	f.api.LoseNextResponse("POST /v1/wireguard/peers")

	if _, err := f.use.AddDevice(t.Context(), "DE", "Berlin", "iPhone"); err == nil {
		t.Fatal("AddDevice succeeded although the answer was lost")
	}
	_, config := f.addDevice(t, "Berlin", "iPhone")

	peers := f.api.Peers(t, email)
	if len(peers) != 1 {
		t.Fatalf("peers = %d, want the one the lost answer created", len(peers))
	}
	if got := publicKeyOf(t, privateKey(t, config)); got != peers[0].PublicKey {
		t.Errorf("config's key belongs to %s, but the peer has %s", got, peers[0].PublicKey)
	}
}

func TestAddDeviceStartsAnewWhenTheRetryIsTooOld(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	f.api.LoseNextResponse("POST /v1/wireguard/peers")
	if _, err := f.use.AddDevice(t.Context(), "DE", "Berlin", "iPhone"); err == nil {
		t.Fatal("AddDevice succeeded although the answer was lost")
	}
	f.api.FailNext("POST /v1/wireguard/peers", http.StatusBadRequest, "request_id_expired")

	got, config := f.addDevice(t, "Berlin", "iPhone")

	// The lost peer's key is gone with the expired request, so the new
	// config has a peer and key of its own.
	peers := f.api.Peers(t, email)
	if len(peers) != 2 || got.Device.PeerID != peers[1].PeerID {
		t.Fatalf("device %s, peers %+v; want a second peer", got.Device.PeerID, peers)
	}
	if k := publicKeyOf(t, privateKey(t, config)); k != peers[1].PublicKey {
		t.Errorf("config's key belongs to %s, but the new peer has %s", k, peers[1].PublicKey)
	}
}

func TestAddDeviceExplainsWhyItGotNoLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   services.ErrorCode
	}{
		{"no free line", http.StatusConflict, "no_free_line", services.ErrNoFreeLine},
		{"unverified", http.StatusForbidden, "identity_required", services.ErrIdentityRequired},
		{"too many devices", http.StatusTooManyRequests, "rate_limited", services.ErrRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUse(t)
			f.offerBerlin()
			f.api.FailNext("POST /v1/wireguard/peers", tc.status, tc.code)

			_, err := f.use.AddDevice(t.Context(), "DE", "Berlin", "iPhone")

			if code := errCode(err); code != tc.want {
				t.Errorf("error = %v, want code %q", err, tc.want)
			}
		})
	}
}

func TestAddDeviceNeedsAName(t *testing.T) {
	f := newUse(t)

	f.offerBerlin()
	for _, name := range []string{"", "   ", strings.Repeat("x", 65)} {
		_, err := f.use.AddDevice(t.Context(), "DE", "Berlin", name)
		if code := errCode(err); code != services.ErrInvalidName {
			t.Errorf("name %q: error = %v, want code %q", name, err, services.ErrInvalidName)
		}
	}
	if peers := f.api.Peers(t, email); len(peers) != 0 {
		t.Errorf("peers = %d, want none", len(peers))
	}
}

func TestSaveConfigWritesTheConfFileOnlyTheUserCanRead(t *testing.T) {
	f := newUse(t)
	dev, config := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	f.saver.path = filepath.Join(t.TempDir(), "chosen.conf")

	saved, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID)
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	if !saved {
		t.Fatal("SaveConfig reports nothing saved")
	}
	b, err := os.ReadFile(f.saver.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != config {
		t.Errorf("saved file:\n%s\nwant the QR code's config:\n%s", b, config)
	}
	if fi, err := os.Stat(f.saver.path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("saved file mode = %v (%v), want 0600: it holds a private key", fi.Mode().Perm(), err)
	}
	if got := f.saver.suggested; !slices.Equal(got, []string{dev.FileName}) || dev.FileName != "Homeport-iPhone.conf" {
		t.Errorf("suggested %q (file name %q), want Homeport-iPhone.conf", got, dev.FileName)
	}
}

// Over an existing file, the save dialog asked first; the old file's
// looser permissions don't survive.
func TestSaveConfigOverAnExistingFile(t *testing.T) {
	f := newUse(t)
	dev, config := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	f.saver.path = filepath.Join(t.TempDir(), "old.conf")
	if err := os.WriteFile(f.saver.path, []byte(strings.Repeat("old config\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	b, _ := os.ReadFile(f.saver.path)
	if string(b) != config {
		t.Errorf("saved file:\n%s\nwant:\n%s", b, config)
	}
	if fi, _ := os.Stat(f.saver.path); fi.Mode().Perm() != 0o600 {
		t.Errorf("saved file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestSaveConfigSuggestsAFileNameWireGuardAccepts(t *testing.T) {
	f := newUse(t)
	tunnel := regexp.MustCompile(`^[A-Za-z0-9_=+.-]{1,15}\.conf$`)

	for _, name := range []string{"Anna's travel router", "Büro-Mac", "📱"} {
		dev, _ := f.addDevice(t, f.offerBerlinCity(), name)
		if !tunnel.MatchString(dev.FileName) || !strings.HasPrefix(dev.FileName, "Homeport") {
			t.Errorf("%q: file name %q, want Homeport… with at most 15 characters WireGuard allows", name, dev.FileName)
		}
	}
}

// The config can go while the save dialog is open, say when the session
// ends; then nothing is saved, rather than an empty file.
func TestSaveConfigAfterTheConfigWentDuringTheDialog(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")
	f.saver.path = filepath.Join(t.TempDir(), "late.conf")
	f.saver.during = func() { f.use.SignOut(t.Context()) }

	saved, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID)

	if code := errCode(err); saved || code != services.ErrConfigGone {
		t.Errorf("SaveConfig = %v, %v; want nothing saved and code %q", saved, err, services.ErrConfigGone)
	}
	if _, err := os.Stat(f.saver.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was written for a config that is gone (%v)", err)
	}
}

func TestSaveConfigCancelledWritesNothing(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")
	f.saver.path = "" // the member pressed Cancel

	saved, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID)
	if err != nil || saved {
		t.Fatalf("SaveConfig = %v, %v; want nothing saved and no error", saved, err)
	}

	// The config is still there to save after all.
	f.saver.path = filepath.Join(t.TempDir(), "later.conf")
	if saved, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID); err != nil || !saved {
		t.Errorf("saving after a cancel = %v, %v; want saved", saved, err)
	}
}

// The private key exists only until the member is done with the config:
// afterwards, nothing can show or save it again.
func TestTheConfigIsShownOnce(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")
	f.saver.path = filepath.Join(t.TempDir(), "late.conf")

	f.use.ForgetConfig(dev.Device.PeerID)
	_, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID)

	if code := errCode(err); code != services.ErrConfigGone {
		t.Errorf("error = %v, want code %q", err, services.ErrConfigGone)
	}
	if _, err := os.Stat(f.saver.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a config was saved after it was forgotten (%v)", err)
	}
}

func TestTheConfigIsGoneAfterARestart(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")
	p, err := profile.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := services.NewUseService(apiclient.New(f.api.URL, f.api.Client()), p, services.UseOptions{
		OpenURL: f.browser.Open, SaveFile: f.saver.Choose,
	})
	f.saver.path = filepath.Join(t.TempDir(), "late.conf")

	_, err = restarted.SaveConfig(t.Context(), dev.Device.PeerID)

	if code := errCode(err); code != services.ErrConfigGone {
		t.Errorf("error = %v, want code %q", err, services.ErrConfigGone)
	}
}

func TestSignOutForgetsTheConfig(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")

	f.use.SignOut(t.Context())
	_, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID)

	if code := errCode(err); code != services.ErrConfigGone {
		t.Errorf("error = %v, want code %q", err, services.ErrConfigGone)
	}
}

func TestDevicesListsTheMembersDevicesNewestFirst(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	phone, _ := f.addDevice(t, "Berlin", "iPhone")
	router, _ := f.addDevice(t, "Berlin", "Travel router")

	got, err := f.use.Devices(t.Context())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	if want := []services.Device{router.Device, phone.Device}; !slices.Equal(got, want) {
		t.Errorf("devices = %+v, want %+v", got, want)
	}
}

// While its line is offline, the gateway moves a device to a backup line,
// and the API reports where its session exits.
func TestDevicesShowTheLineEachDeviceExitsThrough(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	since := time.Date(2026, 11, 14, 13, 2, 10, 0, time.UTC)
	city, country := "Potsdam", "DE"
	f.api.SetExitLine(t, dev.Device.PeerID, &fakeapi.ExitLine{LineID: "line-potsdam", Country: &country, City: &city, Since: since})

	got, err := f.use.Devices(t.Context())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	want := &services.ExitLine{LineID: "line-potsdam", Country: "DE", City: "Potsdam", Since: since}
	if len(got) != 1 || got[0].ExitLine == nil || *got[0].ExitLine != *want {
		t.Fatalf("devices = %+v, want one exiting through %+v", got, want)
	}
}

// The API leaves out where a line is when it doesn't know; a device without
// a session has no exit line.
func TestDevicesExitLineWhereTheAPIKnowsLittle(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	f.api.SetExitLine(t, dev.Device.PeerID, &fakeapi.ExitLine{LineID: "line-x"})

	got, err := f.use.Devices(t.Context())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if e := got[0].ExitLine; e == nil || e.LineID != "line-x" || e.Country != "" || e.City != "" {
		t.Errorf("exit line = %+v, want line-x with no place", e)
	}

	f.api.SetExitLine(t, dev.Device.PeerID, nil)
	got, err = f.use.Devices(t.Context())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if got[0].ExitLine != nil {
		t.Errorf("exit line = %+v, want none without a session", got[0].ExitLine)
	}
}

// offerLyon offers a line in Lyon and returns its id.
func (f *useFixture) offerLyon() string {
	return f.offer("FR", "Lyon", "Orange")
}

// A device moves to another free line in a region and keeps its config:
// nothing to scan again.
func TestUpdateDeviceMovesItToAnotherLineInTheRegion(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	second := f.offer("DE", "Berlin", "M-net")

	got, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, "DE", "Berlin", "")
	if err != nil {
		t.Fatalf("UpdateDevice: %v", err)
	}

	peers := f.api.Peers(t, email)
	if len(peers) != 1 || peers[0].PeerID != dev.Device.PeerID {
		t.Fatalf("peers = %+v, want only %s", peers, dev.Device.PeerID)
	}
	if p := peers[0]; p.LineID != second || p.Failover != services.FailoverCountry {
		t.Errorf("peer on %s with failover %q, want %s and %q", p.LineID, p.Failover, second, services.FailoverCountry)
	}
	want := dev.Device
	want.LineID, want.ISP = second, "M-net"
	if got != want {
		t.Errorf("device = %+v, want %+v", got, want)
	}
}

func TestUpdateDeviceMovesItToAnotherCountry(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
	lyon := f.offerLyon()

	got, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, "FR", "", services.FailoverCity)
	if err != nil {
		t.Fatalf("UpdateDevice: %v", err)
	}

	if got.LineID != lyon || got.Country != "FR" || got.City != "Lyon" || got.Failover != services.FailoverCity {
		t.Errorf("device = %+v, want on %s in Lyon, FR, failover city", got, lyon)
	}
}

func TestUpdateDeviceChangesOnlyItsFailover(t *testing.T) {
	f := newUse(t)
	berlin := f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")

	for _, failover := range []string{services.FailoverCity, services.FailoverOff, services.FailoverCountry} {
		got, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, "", "", failover)
		if err != nil {
			t.Fatalf("UpdateDevice %s: %v", failover, err)
		}
		p := f.api.Peers(t, email)[0]
		if p.Failover != failover || p.LineID != berlin || got.Failover != failover || got.LineID != berlin {
			t.Errorf("failover %s: peer %q on %s, device %q on %s; want %[1]q on %s", failover, p.Failover, p.LineID, got.Failover, got.LineID, berlin)
		}
	}
	if reqs := slices.DeleteFunc(f.api.Requests(), func(r string) bool { return !strings.HasPrefix(r, "PATCH ") }); len(reqs) != 3 {
		t.Errorf("PATCH requests = %q, want 3", reqs)
	}
}

func TestUpdateDeviceNeedsAChange(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")

	for _, tc := range []struct{ country, city, failover string }{{"", "", ""}, {"", "", "everywhere"}, {"", "Berlin", ""}} {
		_, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, tc.country, tc.city, tc.failover)
		if code := errCode(err); code != services.ErrUnexpected {
			t.Errorf("%+v: error = %v, want code %q", tc, err, services.ErrUnexpected)
		}
	}
	if slices.ContainsFunc(f.api.Requests(), func(r string) bool { return strings.HasPrefix(r, "PATCH ") }) {
		t.Errorf("the API got a PATCH: %q", f.api.Requests())
	}
}

// Its own line doesn't count: a move needs another free line.
func TestUpdateDeviceWhenTheRegionHasNoOtherFreeLine(t *testing.T) {
	f := newUse(t)
	dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")

	_, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, "DE", "Berlin", "")

	if code := errCode(err); code != services.ErrNoFreeLine {
		t.Errorf("error = %v, want code %q", err, services.ErrNoFreeLine)
	}
}

func TestUpdateDeviceExplainsWhyItGotNoLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   services.ErrorCode
	}{
		{"no free line", http.StatusConflict, "no_free_line", services.ErrNoFreeLine},
		{"unverified", http.StatusForbidden, "identity_required", services.ErrIdentityRequired},
		{"signed out", http.StatusUnauthorized, "unauthorized", services.ErrSignedOut},
		{"rate limited", http.StatusTooManyRequests, "rate_limited", services.ErrRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUse(t)
			dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
			f.offerLyon()
			f.api.FailNext("PATCH /v1/wireguard/peers/", tc.status, tc.code)

			_, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, "FR", "Lyon", "")

			if code := errCode(err); code != tc.want {
				t.Errorf("error = %v, want code %q", err, tc.want)
			}
		})
	}
}

func TestUpdateDeviceThatWasRemoved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		country  string
		failover string
	}{
		{"new line", "FR", ""},
		{"new failover", "", services.FailoverOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUse(t)
			dev, _ := f.addDevice(t, f.offerBerlinCity(), "iPhone")
			f.offerLyon()
			if err := f.use.RemoveDevice(t.Context(), dev.Device.PeerID); err != nil {
				t.Fatal(err)
			}

			_, err := f.use.UpdateDevice(t.Context(), dev.Device.PeerID, tc.country, "", tc.failover)

			if code := errCode(err); code != services.ErrDeviceGone {
				t.Errorf("error = %v, want code %q", err, services.ErrDeviceGone)
			}
		})
	}
}

func TestDevicesWhenThereAreNoneIsEmpty(t *testing.T) {
	f := newUse(t)

	got, err := f.use.Devices(t.Context())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("devices = %#v, want an empty list", got)
	}
}

func TestRemoveDeviceDeletesItsPeer(t *testing.T) {
	f := newUse(t)
	f.offerBerlin()
	dev, _ := f.addDevice(t, "Berlin", "iPhone")

	if err := f.use.RemoveDevice(t.Context(), dev.Device.PeerID); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}

	if peers := f.api.Peers(t, email); len(peers) != 0 {
		t.Errorf("peers = %+v, want none", peers)
	}
	// Removing it again, say from a second window, is not an error.
	if err := f.use.RemoveDevice(t.Context(), dev.Device.PeerID); err != nil {
		t.Errorf("removing again: %v", err)
	}
	// Its config can't be saved any more.
	if _, err := f.use.SaveConfig(t.Context(), dev.Device.PeerID); errCode(err) != services.ErrConfigGone {
		t.Errorf("SaveConfig after removal = %v, want %q", err, services.ErrConfigGone)
	}
}
