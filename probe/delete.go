package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hstern/go-activesync/eas"
	"github.com/hstern/go-activesync/wbxml"
)

// deleteViaSync deletes one Inbox item with a Sync that carries a Delete command.
//
// The EAS library builds this request itself (ApplyEmailChanges), but without
// the <Options> of the Sync that fetched the item. HCL Traveler answers such a
// request with Sync status 3 (invalid sync key) even though the key is the one
// it just issued (seen in a wire trace; a reset and retry fails the same way).
// So the request here repeats the Options of the Inbox Sync (filter type, body
// preference, window) and only adds the Delete.
func (s *Session) deleteViaSync(ctx context.Context, inbox, serverID string) error { // inbox: the folder of the item
	err := s.syncDeleteOnce(ctx, inbox, serverID)
	if statusIs(err, "Sync", 3) {
		// MS-ASCMD: an invalid key means starting again at key 0. Sync does that
		// (bootstrap, then the first fetch); then the delete is tried once more.
		s.log.Warn("Sync status 3 while deleting the probe mail: rebuilding the Inbox sync key and trying again")
		if serr := s.state.SetSyncKey(ctx, inbox, "0"); serr != nil {
			return serr
		}
		if serr := s.bootstrapFolder(ctx, inbox); serr != nil {
			return serr
		}
		s.rec.setStep(StepSync)
		err = s.syncDeleteOnce(ctx, inbox, serverID)
	}
	return err
}

func (s *Session) syncDeleteOnce(ctx context.Context, inbox, serverID string) error {
	return s.syncCommandOnce(ctx, inbox, serverID, wbxml.E(wbxml.PageAirSync, "Delete",
		wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text(serverID))))
}

// syncCommandOnce sends one Sync request of a folder that carries one client command (Delete or Change), with the
// Options of the Sync that read the folder (see deleteViaSync for why), and keeps the new sync key.
func (s *Session) syncCommandOnce(ctx context.Context, inbox, serverID string, command *wbxml.Element) error {
	_, err := s.syncCommand(ctx, inbox, serverID, command)
	return err
}

// syncCommand is syncCommandOnce that also returns the Collection of the answer, which holds the Responses of an Add. A
// failed command is an error; ref is what the error text calls the item.
func (s *Session) syncCommand(ctx context.Context, inbox, ref string, command *wbxml.Element) (*wbxml.Element, error) {
	key, _ := s.state.SyncKey(ctx, inbox)
	if key == "" || key == "0" {
		if err := s.bootstrapFolder(ctx, inbox); err != nil {
			return nil, err
		}
		s.rec.setStep(StepSync)
		key, _ = s.state.SyncKey(ctx, inbox)
	}

	// The element order of a Collection is fixed by the schema.
	collection := wbxml.E(wbxml.PageAirSync, "Collection",
		wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text(key)),
		wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text(inbox)),
		wbxml.E(wbxml.PageAirSync, "DeletesAsMoves", wbxml.Text("1")),
		wbxml.E(wbxml.PageAirSync, "GetChanges", wbxml.Text("0")),
		wbxml.E(wbxml.PageAirSync, "WindowSize", wbxml.Text(strconv.Itoa(s.window()))),
		wbxml.E(wbxml.PageAirSync, "Options",
			wbxml.E(wbxml.PageAirSync, "FilterType", wbxml.Text("4")), // as in the Inbox Sync
			wbxml.E(wbxml.PageAirSyncBase, "BodyPreference",
				wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text("1")),
				wbxml.E(wbxml.PageAirSyncBase, "TruncationSize", wbxml.Text("256")),
			),
		),
		wbxml.E(wbxml.PageAirSync, "Commands", command),
	)
	body, err := wbxml.Marshal(&wbxml.Document{Root: wbxml.E(wbxml.PageAirSync, "Sync",
		wbxml.E(wbxml.PageAirSync, "Collections", collection))}, wbxml.DefaultRegistry())
	if err != nil {
		return nil, fmt.Errorf("eas: Sync: marshal: %w", err)
	}

	raw, err := s.postWBXML(ctx, "Sync", body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // an empty reply: accepted, nothing to report
	}
	doc, err := wbxml.Unmarshal(raw, wbxml.DefaultRegistry())
	if err != nil || doc.Root == nil {
		return nil, fmt.Errorf("eas: Sync: decode response: %v", err)
	}

	if st := childText(doc.Root, "Status"); st != "" && st != "1" {
		code, _ := strconv.Atoi(st)
		return nil, &eas.StatusError{Command: "Sync", Code: code}
	}
	coll := doc.Root.Find("Collection")
	if coll == nil {
		return nil, fmt.Errorf("eas: Sync: response without a Collection")
	}
	if st := childText(coll, "Status"); st != "" && st != "1" {
		code, _ := strconv.Atoi(st)
		return nil, &eas.StatusError{Command: "Sync", Code: code}
	}
	if k := childText(coll, "SyncKey"); k != "" {
		if err := s.state.SetSyncKey(ctx, inbox, k); err != nil {
			return nil, err
		}
	}
	if resp := coll.Find("Responses"); resp != nil {
		for _, name := range []string{"Delete", "Change", "Add"} {
			if st := childText(resp.Find(name), "Status"); st != "" && st != "1" {
				return nil, fmt.Errorf("%s of %s: status %s", strings.ToLower(name), ref, st)
			}
		}
	}
	return coll, nil
}

// childText returns the text of the direct child element with the name, or "".
func childText(e *wbxml.Element, name string) string {
	if e == nil {
		return ""
	}
	for _, c := range e.Children {
		if el, ok := c.(*wbxml.Element); ok && el.Name == name {
			return el.TextContent()
		}
	}
	return ""
}

// postWBXML sends one EAS command with the same headers and credentials as the
// EAS library and returns the reply body. A non-2xx answer is an eas.HTTPError.
func (s *Session) postWBXML(ctx context.Context, cmd string, body []byte) ([]byte, error) {
	u, err := url.Parse(s.cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("Cmd", cmd)
	q.Set("User", s.cfg.Username)
	q.Set("DeviceId", s.cfg.DeviceID)
	q.Set("DeviceType", s.cfg.DeviceType)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	version := s.server.ProtocolVersion
	if version == "" {
		version = s.cfg.ASVersion
	}
	if version == "" {
		version = "14.1"
	}
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	req.Header.Set("Accept", "application/vnd.ms-sync.wbxml")
	req.Header.Set("MS-ASProtocolVersion", version)
	req.Header.Set("User-Agent", s.cfg.UserAgent)
	if pk, _ := s.state.PolicyKey(ctx); pk != "" {
		req.Header.Set("X-MS-PolicyKey", pk)
	}
	req.SetBasicAuth(s.cfg.Username, s.cfg.Password)

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eas: POST %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(data) > 4096 {
			data = data[:4096]
		}
		return nil, &eas.HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, URL: u.String(), Body: data}
	}
	return data, nil
}
