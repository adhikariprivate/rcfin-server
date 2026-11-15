// Package nfo merges provider metadata into Kodi/Jellyfin NFO documents.
package nfo

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/adhikariprivate/rcfin-server/internal/metadata/provider"
)

type node struct {
	Name     xml.Name
	Attr     []xml.Attr
	Text     string
	Children []*node
	Parts    []nodePart
}

type nodePart struct {
	Text    string
	Comment []byte
	Child   *node
}

type Provenance struct {
	Provider               string
	ProviderID             string
	Language               string
	OverwriteExisting      bool
	ArtworkChecked         bool
	EpisodeMetadataChecked bool
	Confidence             float64
	Evidence               []string
	UpdatedAt              time.Time
}

func (n *node) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: n.Name, Attr: n.Attr}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if n.Parts != nil {
		for _, part := range n.Parts {
			switch {
			case part.Child != nil:
				if err := enc.Encode(part.Child); err != nil {
					return err
				}
			case part.Comment != nil:
				if err := enc.EncodeToken(xml.Comment(part.Comment)); err != nil {
					return err
				}
			default:
				if err := enc.EncodeToken(xml.CharData([]byte(part.Text))); err != nil {
					return err
				}
			}
		}
	} else {
		if n.Text != "" {
			if err := enc.EncodeToken(xml.CharData([]byte(n.Text))); err != nil {
				return err
			}
		}
		for _, child := range n.Children {
			if err := enc.Encode(child); err != nil {
				return err
			}
		}
	}
	return enc.EncodeToken(start.End())
}

// Merge preserves unknown NFO elements and local values by default. Provider
// Values are added only when absent unless overwrite is explicitly requested.
// Malformed input is returned as an error so callers can leave it untouched.
func Merge(existing []byte, meta provider.Metadata, overwrite bool) ([]byte, error) {
	var root *node
	if len(bytes.TrimSpace(existing)) > 0 {
		decoder := xml.NewDecoder(bytes.NewReader(existing))
		var err error
		root, err = readNode(decoder)
		if err != nil {
			return nil, fmt.Errorf("parse existing NFO: %w", err)
		}
		if err := checkDocumentEnd(decoder); err != nil {
			return nil, fmt.Errorf("parse existing NFO: %w", err)
		}
	}
	if root == nil {
		root = &node{Name: xml.Name{Local: rootName(meta.Type)}}
	}
	if root.Name.Local == "" {
		root.Name.Local = rootName(meta.Type)
	}
	if root.Name.Local != rootName(meta.Type) {
		return nil, fmt.Errorf("existing NFO root %q does not match %s metadata", root.Name.Local, meta.Type)
	}
	values := []struct{ name, value string }{
		{"title", meta.Title}, {"originaltitle", meta.OriginalTitle}, {"sorttitle", meta.SortTitle},
		{"year", strconv.Itoa(meta.Year)}, {"premiered", meta.ReleaseDate.Format("2006-01-02")}, {"plot", meta.Overview},
		{"runtime", strconv.Itoa(meta.RuntimeMinutes)},
	}
	if meta.Year == 0 {
		values[3].value = ""
	}
	if meta.RuntimeMinutes <= 0 {
		values[6].value = ""
	}
	if meta.ReleaseDate.IsZero() {
		values[4].value = ""
	}
	if meta.Rating > 0 {
		values = append(values, struct{ name, value string }{"rating", strconv.FormatFloat(float64(meta.Rating), 'f', 1, 32)})
	}
	if meta.ContentRating != "" {
		values = append(values, struct{ name, value string }{"mpaa", meta.ContentRating})
	}
	if meta.Type == provider.Season {
		values = append(values, struct{ name, value string }{"seasonnumber", strconv.Itoa(meta.SeasonNumber)})
	} else if meta.Type == provider.Episode {
		values = append(values, struct{ name, value string }{"season", strconv.Itoa(meta.SeasonNumber)})
		if meta.EpisodeNumber > 0 {
			values = append(values, struct{ name, value string }{"episode", strconv.Itoa(meta.EpisodeNumber)})
		}
	}
	for _, entry := range values {
		if entry.value != "" {
			setText(root, entry.name, entry.value, overwrite)
		}
	}
	if overwrite {
		if len(meta.Genres) > 0 {
			removeChildren(root, "genre")
		}
		if len(meta.Studios) > 0 {
			removeChildren(root, "studio")
		}
		if len(meta.Directors) > 0 {
			removeChildren(root, "director")
		}
		if len(meta.Writers) > 0 {
			removeChildren(root, "credits")
		}
		if len(meta.Actors) > 0 {
			removeChildren(root, "actor")
		}
	}
	for _, genre := range meta.Genres {
		appendUnique(root, "genre", genre)
	}
	for _, studio := range meta.Studios {
		appendUnique(root, "studio", studio)
	}
	for _, director := range meta.Directors {
		appendUnique(root, "director", director)
	}
	for _, writer := range meta.Writers {
		appendUnique(root, "credits", writer)
	}
	actorNames := make([]string, 0, len(meta.Actors))
	for actor := range meta.Actors {
		actorNames = append(actorNames, actor)
	}
	sort.Strings(actorNames)
	for _, name := range actorNames {
		appendActor(root, name, meta.Actors[name])
	}
	ids := make([]string, 0, len(meta.ExternalIDs))
	for id := range meta.ExternalIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		value := strings.TrimSpace(meta.ExternalIDs[id])
		if value == "" {
			continue
		}
		upsertUniqueID(root, id, value, overwrite)
	}
	var out bytes.Buffer
	encoder := xml.NewEncoder(&out)
	encoder.Indent("", "  ")
	if err := encoder.Encode(root); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out.Bytes()...), nil
}

func MergeWithProvenance(existing []byte, meta provider.Metadata, overwrite bool, provenance Provenance) ([]byte, error) {
	data, err := Merge(existing, meta, overwrite)
	if err != nil {
		return nil, err
	}
	root, err := readNode(xml.NewDecoder(bytes.NewReader(data)))
	if err != nil || root == nil {
		if err == nil {
			err = fmt.Errorf("NFO serializer returned no root element")
		}
		return nil, err
	}
	record := child(root, "rcfin")
	if record == nil {
		record = &node{Name: xml.Name{Local: "rcfin"}}
		appendChild(root, record)
	}
	if provenance.Provider != "" {
		setText(record, "provider", provenance.Provider, true)
	}
	if provenance.ProviderID != "" {
		setText(record, "providerid", provenance.ProviderID, true)
	}
	if provenance.Language != "" {
		setText(record, "language", provenance.Language, true)
	}
	setText(record, "overwrite", strconv.FormatBool(provenance.OverwriteExisting), true)
	setText(record, "artworkchecked", strconv.FormatBool(provenance.ArtworkChecked), true)
	setText(record, "episodemetadatachecked", strconv.FormatBool(provenance.EpisodeMetadataChecked), true)
	if provenance.Confidence > 0 {
		setText(record, "confidence", strconv.FormatFloat(provenance.Confidence, 'f', 4, 64), true)
	}
	if !provenance.UpdatedAt.IsZero() {
		setText(record, "updated", provenance.UpdatedAt.UTC().Format(time.RFC3339Nano), true)
	}
	if len(provenance.Evidence) > 0 {
		setText(record, "evidence", strings.Join(provenance.Evidence, "; "), true)
	}
	return encodeRoot(root)
}

func ReadProvenance(data []byte) (Provenance, error) {
	root, err := readNode(xml.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return Provenance{}, err
	}
	if root == nil {
		return Provenance{}, nil
	}
	record := child(root, "rcfin")
	if record == nil {
		return Provenance{}, nil
	}
	result := Provenance{Provider: childText(record, "provider"), ProviderID: childText(record, "providerid"), Language: childText(record, "language"), OverwriteExisting: childText(record, "overwrite") == "true", ArtworkChecked: childText(record, "artworkchecked") == "true", EpisodeMetadataChecked: childText(record, "episodemetadatachecked") == "true"}
	result.Confidence, _ = strconv.ParseFloat(childText(record, "confidence"), 64)
	result.UpdatedAt, _ = time.Parse(time.RFC3339Nano, childText(record, "updated"))
	if evidence := childText(record, "evidence"); evidence != "" {
		result.Evidence = strings.Split(evidence, "; ")
	}
	return result, nil
}

func encodeRoot(root *node) ([]byte, error) {
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out.Bytes()...), nil
}

func removeChildren(root *node, names ...string) {
	remove := map[string]bool{}
	for _, name := range names {
		remove[name] = true
	}
	removed := map[*node]bool{}
	kept := root.Children[:0]
	for _, entry := range root.Children {
		if !remove[entry.Name.Local] {
			kept = append(kept, entry)
		} else {
			removed[entry] = true
		}
	}
	root.Children = kept
	if root.Parts != nil {
		parts := root.Parts[:0]
		for _, part := range root.Parts {
			if part.Child == nil || !removed[part.Child] {
				parts = append(parts, part)
			}
		}
		root.Parts = parts
	}
}
func appendActor(root *node, name, role string) {
	for _, existing := range root.Children {
		if existing.Name.Local == "actor" && strings.EqualFold(strings.TrimSpace(childText(existing, "name")), name) {
			if child(existing, "role") == nil && role != "" {
				appendChild(existing, &node{Name: xml.Name{Local: "role"}, Text: role})
			}
			return
		}
	}
	actor := &node{Name: xml.Name{Local: "actor"}, Children: []*node{{Name: xml.Name{Local: "name"}, Text: name}}}
	if role != "" {
		actor.Children = append(actor.Children, &node{Name: xml.Name{Local: "role"}, Text: role})
	}
	appendChild(root, actor)
}
func childText(root *node, name string) string {
	if n := child(root, name); n != nil {
		return n.Text
	}
	return ""
}

func checkDocumentEnd(dec *xml.Decoder) error {
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			return fmt.Errorf("multiple top-level XML elements")
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return fmt.Errorf("unexpected text after NFO root")
			}
		}
	}
}

func rootName(kind provider.MediaType) string {
	if kind == provider.Show {
		return "tvshow"
	}
	if kind == provider.Season {
		return "season"
	}
	if kind == provider.Episode {
		return "episodedetails"
	}
	return "movie"
}

func readNode(dec *xml.Decoder) (*node, error) {
	for {
		token, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, err
		}
		if start, ok := token.(xml.StartElement); ok {
			return readElement(dec, start)
		}
	}
}
func readElement(dec *xml.Decoder, start xml.StartElement) (*node, error) {
	n := &node{Name: start.Name, Attr: append([]xml.Attr(nil), start.Attr...), Parts: []nodePart{}}
	for {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			child, err := readElement(dec, value)
			if err != nil {
				return nil, err
			}
			appendChild(n, child)
		case xml.CharData:
			text := string(value)
			n.Text += text
			n.Parts = append(n.Parts, nodePart{Text: text})
		case xml.Comment:
			n.Parts = append(n.Parts, nodePart{Comment: append([]byte{}, value...)})
		case xml.EndElement:
			if value.Name == start.Name {
				return n, nil
			}
		}
	}
}
func child(n *node, name string) *node {
	for _, c := range n.Children {
		if c.Name.Local == name {
			return c
		}
	}
	return nil
}
func setText(root *node, name, value string, overwrite bool) {
	current := child(root, name)
	if current == nil {
		appendChild(root, &node{Name: xml.Name{Local: name}, Text: value})
		return
	}
	if overwrite || strings.TrimSpace(current.Text) == "" {
		replaceNodeText(current, value)
	}
}

func replaceNodeText(target *node, value string) {
	target.Text = value
	target.Children = nil
	target.Parts = []nodePart{{Text: value}}
}

func appendChild(parent, child *node) {
	parent.Children = append(parent.Children, child)
	if parent.Parts != nil {
		parent.Parts = append(parent.Parts, nodePart{Child: child})
	}
}

func appendUnique(root *node, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, c := range root.Children {
		if c.Name.Local == name && strings.EqualFold(strings.TrimSpace(c.Text), value) {
			return
		}
	}
	appendChild(root, &node{Name: xml.Name{Local: name}, Text: value})
}
func upsertUniqueID(root *node, kind, value string, overwrite bool) {
	for _, c := range root.Children {
		if c.Name.Local != "uniqueid" {
			continue
		}
		typeValue := ""
		for _, a := range c.Attr {
			if a.Name.Local == "type" {
				typeValue = a.Value
			}
		}
		if strings.EqualFold(typeValue, kind) {
			if overwrite || strings.TrimSpace(c.Text) == "" {
				replaceNodeText(c, value)
			}
			return
		}
	}
	appendChild(root, &node{Name: xml.Name{Local: "uniqueid"}, Attr: []xml.Attr{{Name: xml.Name{Local: "type"}, Value: kind}}, Text: value})
}
