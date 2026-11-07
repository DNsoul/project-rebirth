package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"awgrotator/internal/fleet"
	"awgrotator/internal/proto"
)

// Inbox is what the operator has left for the server: a command and a
// revocation record, still in their encrypted form.
type Inbox struct {
	Command    string
	Revocation string
}

// Publisher is the server's voice and ears: it tells clients where to connect
// and how it is doing, and reads what the operator has asked.
type Publisher interface {
	// PublishConfig signs and publishes a configuration and returns its
	// sequence number.
	PublishConfig(ctx context.Context, body *fleet.ConfigBody) (uint64, error)

	// PublishStatus signs and publishes a status report.
	PublishStatus(ctx context.Context, status StatusBody) error

	// Current reads and fully verifies the configuration that is published.
	Current(ctx context.Context) (*fleet.ConfigBody, uint64, error)

	// ReadInbox returns the operator's command and revocation files.
	ReadInbox(ctx context.Context) (Inbox, error)
}

// GistAPI is the part of the gist client the publisher uses.
type GistAPI interface {
	Get(ctx context.Context, id string) (map[string]string, error)
	Update(ctx context.Context, id string, files map[string]string) error
}

// DocValidity is how long a published document stays valid. A server
// republishes weekly, so this is a margin, not a schedule.
const DocValidity = 60 * 24 * time.Hour

// GistPublisher publishes through a secret gist.
type GistPublisher struct {
	Cfg  *Config
	Gist GistAPI
	J    *JournalStore
	Now  func() time.Time
}

func (p *GistPublisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// PublishConfig implements Publisher.
//
// The document is read back through the same verification a client applies
// before it is uploaded, so a configuration that clients would reject never
// reaches the gist.
func (p *GistPublisher) PublishConfig(ctx context.Context, body *fleet.ConfigBody) (uint64, error) {
	if err := body.Validate(); err != nil {
		return 0, fmt.Errorf("the configuration to publish is not valid: %w", err)
	}
	now := p.now()
	certEnd := time.Unix(p.Cfg.Cert.NotAfter, 0)
	if now.After(certEnd) {
		return 0, errors.New("the certificate has expired: the operator has to issue a new one")
	}

	// Sequence numbers only ever go up, across servers, so the next one is
	// derived from what is actually published as well as from our own record.
	published := uint64(0)
	if files, err := p.Gist.Get(ctx, p.Cfg.GistID); err == nil {
		if blob, ok := files[proto.FileConfig]; ok {
			trust := p.Cfg.Trust()
			if _, st, err := trust.OpenDoc(blob, proto.KindConfig, now, proto.State{}); err == nil {
				published = st.LastSeq
			}
		}
	}
	seq := max(p.J.Read().LastSeq, published) + 1

	raw, err := body.Marshal()
	if err != nil {
		return 0, err
	}
	expires := now.Add(DocValidity)
	if expires.After(certEnd) {
		expires = certEnd
	}
	doc := &proto.Doc{
		Kind:      proto.KindConfig,
		Seq:       seq,
		IssuedAt:  now.Unix(),
		ExpiresAt: expires.Unix(),
		Cert:      p.Cfg.Cert,
		Body:      string(raw),
	}
	blob, err := p.seal(doc)
	if err != nil {
		return 0, err
	}

	trust := p.Cfg.Trust()
	if _, _, err := trust.OpenDoc(blob, proto.KindConfig, now, proto.State{}); err != nil {
		return 0, fmt.Errorf("our own configuration does not pass verification: %w", err)
	}
	if err := p.Gist.Update(ctx, p.Cfg.GistID, map[string]string{proto.FileConfig: blob}); err != nil {
		return 0, fmt.Errorf("upload: %w", err)
	}
	if err := p.J.Update(func(j *Journal) { j.LastSeq = seq }); err != nil {
		return seq, fmt.Errorf("published, but could not record the sequence number: %w", err)
	}
	return seq, nil
}

// PublishStatus implements Publisher.
func (p *GistPublisher) PublishStatus(ctx context.Context, status StatusBody) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	now := p.now()
	expires := now.Add(DocValidity)
	if certEnd := time.Unix(p.Cfg.Cert.NotAfter, 0); expires.After(certEnd) {
		expires = certEnd
	}
	doc := &proto.Doc{
		Kind: proto.KindStatus,
		// A status is a snapshot, not part of a sequence, so the time stands
		// in for one.
		Seq:       uint64(now.Unix()),
		IssuedAt:  now.Unix(),
		ExpiresAt: expires.Unix(),
		Cert:      p.Cfg.Cert,
		Body:      string(raw),
	}
	blob, err := p.seal(doc)
	if err != nil {
		return err
	}
	return p.Gist.Update(ctx, p.Cfg.GistID, map[string]string{proto.FileStatus: blob})
}

func (p *GistPublisher) seal(doc *proto.Doc) (string, error) {
	key, err := p.Cfg.ServerKey()
	if err != nil {
		return "", err
	}
	contentKey, err := p.Cfg.ContentKeyBytes()
	if err != nil {
		return "", err
	}
	return proto.SealDoc(doc, key, contentKey)
}

// Current implements Publisher.
func (p *GistPublisher) Current(ctx context.Context) (*fleet.ConfigBody, uint64, error) {
	files, err := p.Gist.Get(ctx, p.Cfg.GistID)
	if err != nil {
		return nil, 0, err
	}
	blob, ok := files[proto.FileConfig]
	if !ok {
		return nil, 0, errors.New("the gist has no configuration file")
	}
	trust := p.Cfg.Trust()
	doc, st, err := trust.OpenDoc(blob, proto.KindConfig, p.now(), proto.State{})
	if err != nil {
		return nil, 0, fmt.Errorf("the published configuration does not verify: %w", err)
	}
	body, err := fleet.ParseBody([]byte(doc.Body))
	if err != nil {
		return nil, 0, err
	}
	return body, st.LastSeq, nil
}

// ReadInbox implements Publisher.
func (p *GistPublisher) ReadInbox(ctx context.Context) (Inbox, error) {
	files, err := p.Gist.Get(ctx, p.Cfg.GistID)
	if err != nil {
		return Inbox{}, err
	}
	return Inbox{Command: files[proto.FileCommand], Revocation: files[proto.FileRevocation]}, nil
}
