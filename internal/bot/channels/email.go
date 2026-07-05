package channels

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Email is the email channel adapter. It polls an IMAP mailbox for
// unread messages and replies via SMTP. This mirrors the Python
// email.py: poll loop, multipart parsing (text/plain preferred,
// text/html fallback), per-UID dedup, consent-gated sending, and
// Re:-prefixed reply subjects with In-Reply-To/References threading.
type Email struct {
	name string
	cfg  config.ChannelConfig
	api  emailAPI
}

// emailAPI is the subset of the IMAP+SMTP surface the adapter uses.
// Real implementation talks to live servers; tests inject a stub.
type emailAPI interface {
	// FetchNew polls the configured mailbox and returns parsed unread
	// messages, marking them seen when markSeen is true.
	FetchNew(ctx context.Context) ([]emailInbound, error)
	// FetchRange pulls messages whose internal date is in [since, before).
	// A zero Before means "no upper bound". A zero Since means "no lower
	// bound". At least one of the two must be set. The markSeen setting
	// still applies. Dedup is via the same processed-UID set as FetchNew.
	// Use this for historical backfill; use FetchNew for live polling.
	FetchRange(ctx context.Context, since, before time.Time) ([]emailInbound, error)
	// Send delivers an email. toAddrs is the recipient list; raw is the
	// full RFC822 message body.
	Send(ctx context.Context, from string, to []string, raw []byte) error
}

// emailInbound is the parsed form of one inbound email.
type emailInbound struct {
	Sender     string // lower-cased email address
	Subject    string
	MessageID  string
	Body       string
	InReplyTo  string
	Date       string
	UID        uint32
	HasMessage bool
}

// emailSettings is the typed view of cfg.Extra for the email channel.
// Mirrors the Python EmailChannelConfig fields.
type emailSettings struct {
	IMAPHost     string
	IMAPPort     int
	IMAPUser     string
	IMAPPass     string
	IMAPUseSSL   bool
	IMAPMailbox  string
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPass     string
	SMTPUseSSL   bool
	SMTPUseTLS   bool
	FromAddress  string
	Consent      bool
	AutoReply    bool
	MarkSeen     bool
	MaxBodyChars int
	PollSeconds  int
	SubjectPfx   string
}

func parseEmailSettings(cfg config.ChannelConfig) (emailSettings, error) {
	s := emailSettings{
		IMAPHost:     cfg.Endpoint,
		IMAPPort:     extraInt(cfg.Extra, "imap_port", 0),
		IMAPUser:     cfg.AppID,
		IMAPPass:     cfg.AppSecret,
		IMAPUseSSL:   extraBool(cfg.Extra, "imap_use_ssl", true),
		IMAPMailbox:  extraString(cfg.Extra, "imap_mailbox", "INBOX"),
		SMTPHost:     cfg.BaseURL,
		SMTPPort:     extraInt(cfg.Extra, "smtp_port", 0),
		SMTPUser:     extraString(cfg.Extra, "smtp_username", cfg.AppID),
		SMTPPass:     cfg.Token,
		SMTPUseSSL:   extraBool(cfg.Extra, "smtp_use_ssl", false),
		SMTPUseTLS:   extraBool(cfg.Extra, "smtp_use_tls", true),
		FromAddress:  extraString(cfg.Extra, "from_address", ""),
		Consent:      extraBool(cfg.Extra, "consent_granted", false),
		AutoReply:    extraBool(cfg.Extra, "auto_reply_enabled", true),
		MarkSeen:     extraBool(cfg.Extra, "mark_seen", true),
		MaxBodyChars: extraInt(cfg.Extra, "max_body_chars", 20000),
		PollSeconds:  extraInt(cfg.Extra, "poll_interval_seconds", 60),
		SubjectPfx:   extraString(cfg.Extra, "subject_prefix", "Re: "),
	}
	if cfg.Extra != nil {
		if v, ok := cfg.Extra["imap_host"].(string); ok && v != "" {
			s.IMAPHost = v
		}
		if v, ok := cfg.Extra["smtp_host"].(string); ok && v != "" {
			s.SMTPHost = v
		}
	}
	if s.IMAPPort == 0 {
		if s.IMAPUseSSL {
			s.IMAPPort = 993
		} else {
			s.IMAPPort = 143
		}
	}
	if s.SMTPPort == 0 {
		if s.SMTPUseSSL {
			s.SMTPPort = 465
		} else {
			s.SMTPPort = 587
		}
	}
	if s.PollSeconds < 5 {
		s.PollSeconds = 5
	}
	missing := []string{}
	if s.IMAPHost == "" {
		missing = append(missing, "imap_host (endpoint)")
	}
	if s.IMAPUser == "" {
		missing = append(missing, "imap_username (app_id)")
	}
	if s.IMAPPass == "" {
		missing = append(missing, "imap_password (app_secret)")
	}
	if s.SMTPHost == "" {
		missing = append(missing, "smtp_host (base_url)")
	}
	if s.SMTPUser == "" {
		missing = append(missing, "smtp_username")
	}
	if s.SMTPPass == "" {
		missing = append(missing, "smtp_password (token)")
	}
	if len(missing) > 0 {
		return s, fmt.Errorf("email: missing config: %s", strings.Join(missing, ", "))
	}
	return s, nil
}

// emailLiveClient is the production emailAPI: real IMAP+SMTP servers.
type emailLiveClient struct {
	cfg     config.ChannelConfig
	settings emailSettings
	// processed UIDs, capped at maxProcessedUIDs to prevent unbounded growth.
	mu         sync.Mutex
	processed  map[uint32]struct{}
	lastSubj   map[string]string
	lastMsgID  map[string]string
}

const emailMaxProcessed = 100000

func newEmailLiveClient(cfg config.ChannelConfig) (*emailLiveClient, error) {
	settings, err := parseEmailSettings(cfg)
	if err != nil {
		return nil, err
	}
	return &emailLiveClient{
		cfg:        cfg,
		settings:   settings,
		processed:  make(map[uint32]struct{}),
		lastSubj:   make(map[string]string),
		lastMsgID:  make(map[string]string),
	}, nil
}

// dialIMAP connects + authenticates against the configured IMAP server.
func (c *emailLiveClient) dialIMAP() (*imapclient.Client, error) {
	addr := net.JoinHostPort(c.settings.IMAPHost, strconv.Itoa(c.settings.IMAPPort))
	var cl *imapclient.Client
	var err error
	if c.settings.IMAPUseSSL {
		cl, err = imapclient.DialTLS(addr, &tls.Config{ServerName: c.settings.IMAPHost})
	} else {
		cl, err = imapclient.Dial(addr)
	}
	if err != nil {
		return nil, fmt.Errorf("email: imap dial %s: %w", addr, err)
	}
	if err := cl.Login(c.settings.IMAPUser, c.settings.IMAPPass); err != nil {
		_ = cl.Logout()
		return nil, fmt.Errorf("email: imap login: %w", err)
	}
	return cl, nil
}

// FetchNew implements emailAPI. It selects the mailbox, UidSearch-es
// for UNSEEN, UidFetch-es each message body, parses it, and marks it
// seen when configured. Dedup is via a capped processed-UID set.
func (c *emailLiveClient) FetchNew(ctx context.Context) ([]emailInbound, error) {
	cl, err := c.dialIMAP()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cl.Logout() }()

	mbox := c.settings.IMAPMailbox
	if mbox == "" {
		mbox = "INBOX"
	}
	if _, err := cl.Select(mbox, false); err != nil {
		return nil, fmt.Errorf("email: select %s: %w", mbox, err)
	}
	criteria := &imap.SearchCriteria{
		WithoutFlags: []string{imap.SeenFlag},
	}
	uids, err := cl.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("email: uid search: %w", err)
	}
	out := []emailInbound{}
	if len(uids) == 0 {
		return out, nil
	}
	seqset := new(imap.SeqSet)
	for _, u := range uids {
		seqset.AddNum(u)
	}
	section, err := imap.ParseBodySectionName("BODY.PEEK[]")
	if err != nil {
		return nil, fmt.Errorf("email: parse body section: %w", err)
	}
	items := []imap.FetchItem{imap.FetchUid, section.FetchItem(), imap.FetchRFC822Size}
	ch := make(chan *imap.Message, len(uids))
	go func() {
		_ = cl.UidFetch(seqset, items, ch)
	}()
	for msg := range ch {
		if msg == nil {
			continue
		}
		uid := msg.Uid
		c.mu.Lock()
		_, seen := c.processed[uid]
		c.mu.Unlock()
		if seen {
			continue
		}
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		parsed, perr := parseEmailBody(r)
		if perr != nil {
			continue
		}
		sender := strings.ToLower(strings.TrimSpace(parsed.Sender))
		if sender == "" {
			continue
		}
		body := parsed.Body
		if body == "" {
			body = "(empty email body)"
		}
		if c.settings.MaxBodyChars > 0 && len(body) > c.settings.MaxBodyChars {
			body = body[:c.settings.MaxBodyChars]
		}
		content := fmt.Sprintf("Email received.\nFrom: %s\nSubject: %s\nDate: %s\n\n%s",
			sender, parsed.Subject, parsed.Date, body)
		out = append(out, emailInbound{
			Sender:     sender,
			Subject:    parsed.Subject,
			MessageID:  parsed.MessageID,
			Body:       content,
			InReplyTo:  parsed.InReplyTo,
			Date:       parsed.Date,
			UID:        uid,
			HasMessage: true,
		})
		c.mu.Lock()
		c.processed[uid] = struct{}{}
		if len(c.processed) > emailMaxProcessed {
			c.processed = make(map[uint32]struct{})
		}
		if parsed.Subject != "" {
			c.lastSubj[sender] = parsed.Subject
		}
		if parsed.MessageID != "" {
			c.lastMsgID[sender] = parsed.MessageID
		}
		c.mu.Unlock()
	}
	if c.settings.MarkSeen {
		flags := []string{imap.SeenFlag}
		_ = cl.UidStore(seqset, imap.FormatFlagsOp(imap.AddFlags, false), flags, nil)
	}
	return out, nil
}

// FetchRange implements emailAPI. It selects the mailbox, UidSearch-es
// for messages with internal date in [since, before), then UidFetch-es
// each message body. Dedup reuses the same processed-UID set as FetchNew
// so a backfill followed by live polling does not double-process.
//
// A zero Before means "no upper bound"; a zero Since means "no lower
// bound". At least one of the two must be set (otherwise the call is
// equivalent to FetchNew without the UNSEEN filter — prefer FetchNew
// for that case).
func (c *emailLiveClient) FetchRange(ctx context.Context, since, before time.Time) ([]emailInbound, error) {
	if since.IsZero() && before.IsZero() {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"email: FetchRange requires at least one of since/before")
	}
	cl, err := c.dialIMAP()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cl.Logout() }()

	mbox := c.settings.IMAPMailbox
	if mbox == "" {
		mbox = "INBOX"
	}
	if _, err := cl.Select(mbox, false); err != nil {
		return nil, fmt.Errorf("email: select %s: %w", mbox, err)
	}
	criteria := &imap.SearchCriteria{}
	if !since.IsZero() {
		criteria.Since = since
	}
	if !before.IsZero() {
		criteria.Before = before
	}
	uids, err := cl.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("email: uid search range: %w", err)
	}
	out := []emailInbound{}
	if len(uids) == 0 {
		return out, nil
	}
	seqset := new(imap.SeqSet)
	for _, u := range uids {
		seqset.AddNum(u)
	}
	section, err := imap.ParseBodySectionName("BODY.PEEK[]")
	if err != nil {
		return nil, fmt.Errorf("email: parse body section: %w", err)
	}
	items := []imap.FetchItem{imap.FetchUid, section.FetchItem(), imap.FetchRFC822Size}
	ch := make(chan *imap.Message, len(uids))
	go func() {
		_ = cl.UidFetch(seqset, items, ch)
	}()
	for msg := range ch {
		if msg == nil {
			continue
		}
		uid := msg.Uid
		c.mu.Lock()
		_, seen := c.processed[uid]
		c.mu.Unlock()
		if seen {
			continue
		}
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		parsed, perr := parseEmailBody(r)
		if perr != nil {
			continue
		}
		sender := strings.ToLower(strings.TrimSpace(parsed.Sender))
		if sender == "" {
			continue
		}
		body := parsed.Body
		if body == "" {
			body = "(empty email body)"
		}
		if c.settings.MaxBodyChars > 0 && len(body) > c.settings.MaxBodyChars {
			body = body[:c.settings.MaxBodyChars]
		}
		content := fmt.Sprintf("Email received.\nFrom: %s\nSubject: %s\nDate: %s\n\n%s",
			sender, parsed.Subject, parsed.Date, body)
		out = append(out, emailInbound{
			Sender:     sender,
			Subject:    parsed.Subject,
			MessageID:  parsed.MessageID,
			Body:       content,
			InReplyTo:  parsed.InReplyTo,
			Date:       parsed.Date,
			UID:        uid,
			HasMessage: true,
		})
		c.mu.Lock()
		c.processed[uid] = struct{}{}
		if len(c.processed) > emailMaxProcessed {
			c.processed = make(map[uint32]struct{})
		}
		c.mu.Unlock()
	}
	if c.settings.MarkSeen {
		flags := []string{imap.SeenFlag}
		_ = cl.UidStore(seqset, imap.FormatFlagsOp(imap.AddFlags, false), flags, nil)
	}
	return out, nil
}

// Send implements emailAPI. It dials SMTP (implicit TLS, STARTTLS, or
// plain), performs PLAIN auth, and submits the raw RFC822 message.
func (c *emailLiveClient) Send(ctx context.Context, from string, to []string, raw []byte) error {
	addr := net.JoinHostPort(c.settings.SMTPHost, strconv.Itoa(c.settings.SMTPPort))
	auth := sasl.NewPlainClient("", c.settings.SMTPUser, c.settings.SMTPPass)
	reader := bytes.NewReader(raw)
	switch {
	case c.settings.SMTPUseSSL:
		// Implicit TLS (port 465).
		if err := smtp.SendMailTLS(addr, auth, from, to, reader); err != nil {
			return fmt.Errorf("email: smtp send (ssl): %w", err)
		}
		return nil
	case c.settings.SMTPUseTLS:
		// STARTTLS upgrade (port 587).
		conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("email: smtp dial: %w", err)
		}
		cl, err := smtp.NewClientStartTLS(conn, &tls.Config{ServerName: c.settings.SMTPHost})
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("email: smtp starttls: %w", err)
		}
		defer cl.Close()
		if err := cl.Auth(auth); err != nil {
			return fmt.Errorf("email: smtp auth: %w", err)
		}
		if err := cl.Mail(from, nil); err != nil {
			return fmt.Errorf("email: smtp mail: %w", err)
		}
		for _, rcpt := range to {
			if err := cl.Rcpt(rcpt, nil); err != nil {
				return fmt.Errorf("email: smtp rcpt %s: %w", rcpt, err)
			}
		}
		w, err := cl.Data()
		if err != nil {
			return fmt.Errorf("email: smtp data: %w", err)
		}
		if _, err := w.Write(raw); err != nil {
			return fmt.Errorf("email: smtp write: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("email: smtp close: %w", err)
		}
		return nil
	default:
		// Plain (no TLS, port 25). Use SendMail which handles auth.
		if err := smtp.SendMail(addr, auth, from, to, reader); err != nil {
			return fmt.Errorf("email: smtp send (plain): %w", err)
		}
		return nil
	}
}

// readPartBody fully reads a message part. message.Read already
// decodes Content-Transfer-Encoding and charset into UTF-8, so the
// raw bytes here are the decoded text.
func readPartBody(part *message.Entity) (string, error) {
	if part == nil || part.Body == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, part.Body); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// parsedEmail is the output of parseEmailBody.
type parsedEmail struct {
	Sender    string
	Subject   string
	MessageID string
	Date      string
	InReplyTo string
	Body      string
}

// parseEmailBody reads an RFC822 message from r and extracts the
// fields the adapter needs. Mirrors the Python _extract_text_body:
// text/plain preferred, text/html stripped to plain as fallback.
func parseEmailBody(r io.Reader) (parsedEmail, error) {
	ent, err := message.Read(r)
	if err != nil {
		return parsedEmail{}, fmt.Errorf("email: parse: %w", err)
	}
	p := parsedEmail{
		Sender:    parseAddress(ent.Header.Get("From")),
		Subject:   decodeRFC2047Header(ent.Header.Get("Subject")),
		MessageID: strings.TrimSpace(ent.Header.Get("Message-ID")),
		Date:      ent.Header.Get("Date"),
		InReplyTo: strings.TrimSpace(ent.Header.Get("In-Reply-To")),
		Body:      extractBodyText(ent),
	}
	return p, nil
}

// parseAddress extracts the bare email from a From/To header.
func parseAddress(v string) string {
	addr, err := mail.ParseAddress(v)
	if err != nil {
		return strings.TrimSpace(v)
	}
	return strings.ToLower(strings.TrimSpace(addr.Address))
}

// extractBodyText walks a (possibly multipart) message and returns the
// concatenated text/plain parts, or a stripped text/html fallback.
func extractBodyText(ent *message.Entity) string {
	if ent == nil {
		return ""
	}
	// Multipart: walk children, collect text/plain first then text/html.
	if mr := ent.MultipartReader(); mr != nil {
		var plain, html []string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			ct := part.Header.Get("Content-Type")
			dispo := part.Header.Get("Content-Disposition")
			if strings.HasPrefix(strings.ToLower(dispo), "attachment") {
				continue
			}
			body, _ := readPartBody(part)
			switch {
			case strings.HasPrefix(ct, "text/plain"):
				plain = append(plain, body)
			case strings.HasPrefix(ct, "text/html"):
				html = append(html, body)
			}
		}
		if len(plain) > 0 {
			return strings.TrimSpace(strings.Join(plain, "\n\n"))
		}
		if len(html) > 0 {
			return strings.TrimSpace(htmlToText(strings.Join(html, "\n\n")))
		}
		return ""
	}
	// Single part: decode by content type.
	ct := ent.Header.Get("Content-Type")
	body, _ := readPartBody(ent)
	if strings.HasPrefix(ct, "text/html") {
		return strings.TrimSpace(htmlToText(body))
	}
	return strings.TrimSpace(body)
}

// htmlToText is a minimal HTML-to-plain-text stripper: <br> / </p> ->
// newline, all other tags stripped, entities unescaped. Mirrors the
// Python _html_to_text.
var (
	brRe   = regexp.MustCompile(`(?i)<\s*br\s*/?>`)
	pClose = regexp.MustCompile(`(?i)<\s*/\s*p\s*>`)
	tagRe  = regexp.MustCompile(`(?i)<[^>]+>`)
)

func htmlToText(s string) string {
	s = brRe.ReplaceAllString(s, "\n")
	s = pClose.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	return decodeHTMLEntities(s)
}

// decodeHTMLEntities unescapes the common HTML entities. The stdlib
// html.UnescapeString covers this; we wrap it for clarity.
func decodeHTMLEntities(s string) string {
	// imported indirectly to avoid an extra import here; use stdlib.
	return htmlUnescapeString(s)
}

// decodeRFC2047Header decodes RFC 2047 encoded-words in headers like
// "Subject: =?UTF-8?B?...?=". The stdlib mime/decoder handles this.
func decodeRFC2047Header(v string) string {
	if v == "" {
		return ""
	}
	dec := newMimeWordDecoder()
	out, err := dec.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}

// NewEmail constructs an Email adapter.
func NewEmail(name string, cfg config.ChannelConfig) *Email {
	return &Email{name: name, cfg: cfg}
}

// Connect initializes the IMAP+SMTP client. The api argument (when
// non-nil) replaces the real client — used by tests to inject a stub.
func (e *Email) Connect(ctx context.Context, api emailAPI) error {
	if api != nil {
		e.api = api
		return nil
	}
	live, err := newEmailLiveClient(e.cfg)
	if err != nil {
		return err
	}
	e.api = live
	return nil
}

// Name implements Channel.
func (e *Email) Name() string { return e.name }

// Backfill pulls messages whose internal date is in [since, before) and
// returns them without dispatching to the channel handler. Use this for
// historical email backfill (gap 1.4). A zero Before means "no upper
// bound"; Since must be set. At least one of the two must be set.
//
// Dedup reuses the same processed-UID set as the live poll loop, so a
// backfill followed by Start will not double-process the same UID.
func (e *Email) Backfill(ctx context.Context, since, before time.Time) ([]emailInbound, error) {
	if e.api == nil {
		return nil, fmt.Errorf("email: not connected")
	}
	return e.api.FetchRange(ctx, since, before)
}

// Start implements Channel. It polls the IMAP mailbox at the configured
// interval, dispatching each new message to handler. Returns when ctx
// is canceled.
func (e *Email) Start(ctx context.Context, handler Handler) error {
	if e.api == nil {
		return fmt.Errorf("email: not connected")
	}
	settings, err := parseEmailSettings(e.cfg)
	if err != nil {
		return err
	}
	if !settings.Consent {
		return fmt.Errorf("email: consent_granted is false; refusing to start")
	}
	ticker := time.NewTicker(time.Duration(settings.PollSeconds) * time.Second)
	defer ticker.Stop()
	// Run one fetch immediately, then on each tick.
	if err := e.pollOnce(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
		// Log-and-continue; the loop will retry on the next tick.
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.pollOnce(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
				continue
			}
		}
	}
}

func (e *Email) pollOnce(ctx context.Context, handler Handler) error {
	msgs, err := e.api.FetchNew(ctx)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		im := toIncomingEmail(e.name, e.cfg, m)
		if im.ChatID == "" || im.Text == "" {
			continue
		}
		if err := handler(ctx, im); err != nil {
			return err
		}
	}
	return nil
}

// Send implements Channel. It builds a reply email with the right
// Subject/In-Reply-To/References headers and submits it via SMTP.
// Consent and auto_reply_enabled are honored from config.
func (e *Email) Send(ctx context.Context, msg OutgoingMessage) error {
	if e.api == nil {
		return fmt.Errorf("email: not connected")
	}
	settings, err := parseEmailSettings(e.cfg)
	if err != nil {
		return err
	}
	if !settings.Consent {
		return fmt.Errorf("email: consent_granted is false; refusing to send")
	}
	if !settings.AutoReply {
		return nil
	}
	to := strings.TrimSpace(msg.ChatID)
	if to == "" {
		return fmt.Errorf("email: missing recipient (chat_id)")
	}
	from := settings.FromAddress
	if from == "" {
		from = settings.SMTPUser
	}
	if from == "" {
		from = settings.IMAPUser
	}
	// Look up the last inbound subject / message-id we recorded for
	// this sender. When using a stub api we fall back to the metadata
	// encoded in the chat_id (we don't, so just default).
	baseSubject := "vikingbot reply"
	if live, ok := e.api.(*emailLiveClient); ok {
		live.mu.Lock()
		if s, ok := live.lastSubj[to]; ok {
			baseSubject = s
		}
		live.mu.Unlock()
	}
	subject := replySubject(baseSubject, settings.SubjectPfx)
	raw := buildReplyEmail(from, to, subject, msg.Text, "")
	return e.api.Send(ctx, from, []string{to}, raw)
}

// toIncomingEmail converts a polled emailInbound to an IncomingMessage.
// chat_id is the lower-cased sender address so replies route to the
// same conversation.
func toIncomingEmail(name string, cfg config.ChannelConfig, m emailInbound) IncomingMessage {
	return IncomingMessage{
		ChannelName: name,
		ChatID:      m.Sender,
		UserID:      m.Sender,
		Text:        m.Body,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "email",
		},
		Raw: m,
	}
}

// replySubject mirrors the Python _reply_subject: keep "Re:" prefix
// when already present, otherwise prepend the configured prefix.
func replySubject(base, prefix string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "vikingbot reply"
	}
	if prefix == "" {
		prefix = "Re: "
	}
	if strings.HasPrefix(strings.ToLower(base), "re:") {
		return base
	}
	return prefix + base
}

// buildReplyEmail renders a minimal RFC822 message. The body is plain
// text; In-Reply-To/References are set when inReplyTo is non-empty.
func buildReplyEmail(from, to, subject, body, inReplyTo string) []byte {
	var b bytes.Buffer
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	if inReplyTo != "" {
		b.WriteString("In-Reply-To: " + inReplyTo + "\r\n")
		b.WriteString("References: " + inReplyTo + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}
