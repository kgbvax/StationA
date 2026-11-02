// Package qrz is a client for the QRZ.com XML callbook API
// (https://www.qrz.com/XML/current_spec.html): a session key from the
// operator's QRZ login, then one request per callsign. The password and the
// session key are held here only: they are never logged and never part of
// an error (the request URL carries them, so URL errors are unwrapped).
package qrz

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is the XML interface endpoint.
const DefaultAPI = "https://xmldata.qrz.com/xml/current/"

var (
	// ErrAuth: QRZ refused the login. Do not retry soon.
	ErrAuth = errors.New("QRZ rejected the login")
	// ErrNotFound: the callsign is not in the callbook.
	ErrNotFound = errors.New("callsign not in the QRZ callbook")
)

// Record is the part of a callbook entry chase-queue publishes. Street
// address, e-mail and birth date are left out on purpose.
type Record struct {
	Call    string
	Name    string // name_fmt, else first name + name, else nickname
	City    string // addr2
	State   string
	Country string
	Grid    string
	Lat     float64
	Lon     float64
	HasLL   bool
	Image   string // URL of the operator's picture
	DXCC    int
	CQZone  int
	ITUZone int
}

// Client looks callsigns up. It is safe for concurrent use; requests are
// serialised so one session is shared.
type Client struct {
	API      string
	User     string
	Password string
	Agent    string
	HTTP     *http.Client

	mu  sync.Mutex
	key string
	sub string // the subscription expiry as QRZ reports it ("non-subscriber" = name/address only)
}

// Subscription is QRZ's SubExp of the current session ("" before login).
func (c *Client) Subscription() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sub
}

// Lookup returns the callbook entry of call. A lapsed session is renewed once.
func (c *Client) Lookup(ctx context.Context, call string) (Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if c.key == "" {
			if err := c.loginLocked(ctx); err != nil {
				return Record{}, err
			}
		}
		db, err := c.get(ctx, url.Values{"s": {c.key}, "callsign": {call}})
		if err != nil {
			return Record{}, err
		}
		if db.Session.Key != "" {
			c.key = db.Session.Key
		}
		msg := strings.TrimSpace(db.Session.Error)
		switch {
		case db.Callsign != nil:
			return db.Callsign.record(), nil
		case strings.HasPrefix(strings.ToLower(msg), "not found"):
			return Record{}, ErrNotFound
		case attempt == 0 && sessionLapsed(msg, db.Session.Key):
			c.key = ""
			continue
		case msg != "":
			return Record{}, fmt.Errorf("QRZ: %s", msg)
		default:
			return Record{}, ErrNotFound
		}
	}
}

func sessionLapsed(msg, key string) bool {
	m := strings.ToLower(msg)
	return key == "" || strings.Contains(m, "session") || strings.Contains(m, "invalid key")
}

func (c *Client) loginLocked(ctx context.Context) error {
	if c.User == "" || c.Password == "" {
		return ErrAuth
	}
	agent := c.Agent
	if agent == "" {
		agent = "chase-queue"
	}
	db, err := c.get(ctx, url.Values{"username": {c.User}, "password": {c.Password}, "agent": {agent}})
	if err != nil {
		return err
	}
	if db.Session.Key == "" {
		// "Username/password incorrect", "Invalid username", ...
		return ErrAuth
	}
	c.key, c.sub = db.Session.Key, strings.TrimSpace(db.Session.SubExp)
	return nil
}

func (c *Client) get(ctx context.Context, q url.Values) (database, error) {
	api := c.API
	if api == "" {
		api = DefaultAPI
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?"+q.Encode(), nil)
	if err != nil {
		return database{}, errors.New("QRZ: bad API address")
	}
	resp, err := hc.Do(req)
	if err != nil {
		// *url.Error quotes the URL, which carries the password or the key
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return database{}, fmt.Errorf("QRZ request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return database{}, fmt.Errorf("QRZ: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return database{}, fmt.Errorf("QRZ read failed: %w", err)
	}
	return Parse(body)
}

// Parse reads a QRZDatabase document.
func Parse(body []byte) (database, error) {
	var db database
	if err := xml.Unmarshal(body, &db); err != nil {
		return database{}, fmt.Errorf("QRZ: unreadable reply: %w", err)
	}
	return db, nil
}

type database struct {
	XMLName  xml.Name  `xml:"QRZDatabase"`
	Callsign *callsign `xml:"Callsign"`
	Session  session   `xml:"Session"`
}

type session struct {
	Key    string `xml:"Key"`
	Error  string `xml:"Error"`
	SubExp string `xml:"SubExp"`
}

type callsign struct {
	Call     string `xml:"call"`
	FName    string `xml:"fname"`
	Name     string `xml:"name"`
	NameFmt  string `xml:"name_fmt"`
	Nickname string `xml:"nickname"`
	Addr2    string `xml:"addr2"`
	State    string `xml:"state"`
	Country  string `xml:"country"`
	Grid     string `xml:"grid"`
	Lat      string `xml:"lat"`
	Lon      string `xml:"lon"`
	Image    string `xml:"image"`
	DXCC     string `xml:"dxcc"`
	CQZone   string `xml:"cqzone"`
	ITUZone  string `xml:"ituzone"`
}

func (c callsign) record() Record {
	r := Record{Call: strings.ToUpper(strings.TrimSpace(c.Call)), City: strings.TrimSpace(c.Addr2), State: strings.TrimSpace(c.State),
		Country: strings.TrimSpace(c.Country), Grid: strings.TrimSpace(c.Grid), Image: strings.TrimSpace(c.Image)}
	switch {
	case strings.TrimSpace(c.NameFmt) != "":
		r.Name = strings.TrimSpace(c.NameFmt)
	case strings.TrimSpace(c.FName+" "+c.Name) != "":
		r.Name = strings.TrimSpace(strings.TrimSpace(c.FName) + " " + strings.TrimSpace(c.Name))
	default:
		r.Name = strings.TrimSpace(c.Nickname)
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(c.Lat), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(c.Lon), 64)
	if err1 == nil && err2 == nil && (lat != 0 || lon != 0) {
		r.Lat, r.Lon, r.HasLL = lat, lon, true
	}
	r.DXCC, _ = strconv.Atoi(strings.TrimSpace(c.DXCC))
	r.CQZone, _ = strconv.Atoi(strings.TrimSpace(c.CQZone))
	r.ITUZone, _ = strconv.Atoi(strings.TrimSpace(c.ITUZone))
	return r
}
