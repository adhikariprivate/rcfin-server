package jellyfin

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
)

var srtTimestamp = regexp.MustCompile(`(?m)(\d{2,}:\d{2}:\d{2}),(\d{3})`)

func addSubtitleDeliveryToken(sources []JFMediaSources, token string) {
	if token == "" {
		return
	}
	for i := range sources {
		for j := range sources[i].MediaStreams {
			stream := &sources[i].MediaStreams[j]
			if stream.Type != "Subtitle" || stream.DeliveryUrl == "" {
				continue
			}
			deliveryURL, err := url.Parse(stream.DeliveryUrl)
			if err != nil {
				continue
			}
			query := deliveryURL.Query()
			query.Set("api_key", token)
			deliveryURL.RawQuery = query.Encode()
			stream.DeliveryUrl = deliveryURL.String()
		}
	}
}

func srtToVTT(srt []byte) []byte {
	srt = bytes.TrimPrefix(srt, []byte{0xef, 0xbb, 0xbf})
	converted := srtTimestamp.ReplaceAll(srt, []byte(`${1}.${2}`))
	return append(append([]byte("WEBVTT\n\n"), converted...), '\n')
}

func (j *Jellyfin) subtitleStreamHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	vars := mux.Vars(r)
	routeItemID := vars["itemid"]
	itemID := trimPrefix(routeItemID)
	c, item := j.collections.GetItemByID(itemID)
	if item == nil || c == nil || !j.userCanAccessCollection(r.Context(), reqCtx.User.ID, c.ID) {
		http.NotFound(w, r)
		return
	}
	if vars["mediasourceid"] != item.ID() {
		http.NotFound(w, r)
		return
	}
	if _, isEpisode := item.(*collection.Episode); isEpisode != strings.HasPrefix(routeItemID, itemprefix_episode) {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(vars["index"])
	if err != nil || index < 2 || index-2 >= len(collection.SidecarSubtitles(item)) {
		http.NotFound(w, r)
		return
	}
	sub := collection.SidecarSubtitles(item)[index-2]
	requestedFormat := strings.ToLower(vars["format"])
	ext := strings.ToLower(path.Ext(sub.Path))
	if requestedFormat != "srt" && requestedFormat != "vtt" {
		http.Error(w, "unsupported subtitle format", http.StatusNotFound)
		return
	}
	if requestedFormat == "srt" && ext != ".srt" {
		http.Error(w, "subtitle format conversion is unsupported", http.StatusNotAcceptable)
		return
	}
	if requestedFormat == "vtt" && ext == ".srt" {
		data, err := c.Storage.ReadFile(path.Join(item.Path(), sub.Path))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		content := srtToVTT(data)
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		http.ServeContent(w, r, path.Base(sub.Path)+".vtt", time.Time{}, bytes.NewReader(content))
		return
	}
	f, err := c.Storage.OpenFile(path.Join(item.Path(), sub.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	contentType := "text/vtt; charset=utf-8"
	if ext == ".srt" {
		contentType = "application/x-subrip; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, fmt.Sprintf("unable to seek subtitle: %v", err), http.StatusInternalServerError)
		return
	}
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
