package chaturbate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/samber/lo"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

var roomDossierRegexp = regexp.MustCompile(`window\.initialRoomDossier = "(.*?)"`)

type Client struct {
	Req *internal.Req
}

func NewClient() *Client {
	return &Client{Req: internal.NewReq()}
}

func (c *Client) GetStream(ctx context.Context, username string) (*Stream, error) {
	return FetchStream(ctx, c.Req, username)
}

func FetchStream(ctx context.Context, client *internal.Req, username string) (*Stream, error) {
	body, err := client.Get(ctx, fmt.Sprintf("%s%s", server.Config.Domain, username))
	if err != nil {
		return nil, fmt.Errorf("failed to get page body: %w", err)
	}
	if !strings.Contains(body, "playlist.m3u8") && !strings.Contains(body, "llhls.m3u8") {
		return nil, internal.ErrChannelOffline
	}
	return ParseStream(body)
}

func ParseStream(body string) (*Stream, error) {
	matches := roomDossierRegexp.FindStringSubmatch(body)
	if len(matches) == 0 {
		return nil, errors.New("room dossier not found")
	}
	sourceData, err := strconv.Unquote(strings.Replace(strconv.Quote(matches[1]), `\\u`, `\u`, -1))
	if err != nil {
		return nil, fmt.Errorf("failed to decode unicode: %w", err)
	}
	var room struct {
		HLSSource string `json:"hls_source"`
	}
	if err := json.Unmarshal([]byte(sourceData), &room); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}
	return &Stream{HLSSource: room.HLSSource}, nil
}

type Stream struct {
	HLSSource string
}

func (s *Stream) GetPlaylist(ctx context.Context, resolution, framerate int) (*Playlist, error) {
	return FetchPlaylist(ctx, s.HLSSource, resolution, framerate)
}

func FetchPlaylist(ctx context.Context, hlsSource string, resolution, framerate int) (*Playlist, error) {
	if hlsSource == "" {
		return nil, errors.New("HLS source is empty")
	}
	resp, err := internal.NewReq().Get(ctx, hlsSource)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HLS source: %w", err)
	}
	return ParsePlaylist(resp, hlsSource, resolution, framerate)
}

func ParsePlaylist(resp, hlsSource string, resolution, framerate int) (*Playlist, error) {
	p, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		return nil, fmt.Errorf("failed to decode m3u8 playlist: %w", err)
	}
	masterPlaylist, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, errors.New("invalid master playlist format")
	}

	// Parse EXT-X-MEDIA:TYPE=AUDIO URI directly from raw string —
	// the grafov library does not reliably populate Variant.Alternatives.
	audioURI := extractAudioURI(resp)

	return PickPlaylist(masterPlaylist, hlsSource, audioURI, resolution, framerate)
}

// extractAudioURI scans a raw HLS master playlist for an EXT-X-MEDIA:TYPE=AUDIO URI.
func extractAudioURI(playlist string) string {
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") || !strings.Contains(line, "TYPE=AUDIO") {
			continue
		}
		if idx := strings.Index(line, `URI="`); idx >= 0 {
			rest := line[idx+5:]
			if end := strings.Index(rest, `"`); end >= 0 {
				return rest[:end]
			}
		}
	}
	return ""
}

// Playlist represents an HLS playlist containing variant streams.
type Playlist struct {
	PlaylistURL          string
	RootURL              string
	Resolution           int
	Framerate            int
	InitSegmentData      []byte // video moov box
	lastInitURI          string
	AudioPlaylistURL     string
	AudioInitSegmentData []byte // audio moov box
	audioLastInitURI     string
}

type Resolution struct {
	Framerate map[int]string
	Width     int
}

// PickPlaylist selects the best matching variant and resolves the audio rendition URL.
func PickPlaylist(masterPlaylist *m3u8.MasterPlaylist, baseURL, audioURI string, resolution, framerate int) (*Playlist, error) {
	resolutions := map[int]*Resolution{}

	for _, v := range masterPlaylist.Variants {
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("parse resolution: %w", err)
		}
		framerateVal := 30
		if strings.Contains(v.Name, "FPS:60.0") {
			framerateVal = 60
		}
		if _, exists := resolutions[width]; !exists {
			resolutions[width] = &Resolution{Framerate: map[int]string{}, Width: width}
		}
		resolutions[width].Framerate[framerateVal] = v.URI
	}

	variant, exists := resolutions[resolution]
	if !exists {
		candidates := lo.Filter(lo.Values(resolutions), func(r *Resolution, _ int) bool {
			return r.Width < resolution
		})
		variant = lo.MaxBy(candidates, func(a, b *Resolution) bool {
			return a.Width > b.Width
		})
	}
	if variant == nil {
		return nil, fmt.Errorf("resolution not found")
	}

	var (
		finalResolution = variant.Width
		finalFramerate  = framerate
	)
	playlistURI, exists := variant.Framerate[framerate]
	if !exists {
		for fr, u := range variant.Framerate {
			playlistURI = u
			finalFramerate = fr
			break
		}
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	variantRef, err := url.Parse(playlistURI)
	if err != nil {
		return nil, fmt.Errorf("parse variant URL: %w", err)
	}
	resolvedURL := base.ResolveReference(variantRef).String()
	lastSlash := strings.LastIndex(resolvedURL, "/")
	rootURL := resolvedURL[:lastSlash+1]

	// Resolve audio rendition URL
	var audioPlaylistURL string
	if audioURI != "" {
		audioRef, err := url.Parse(audioURI)
		if err == nil {
			audioPlaylistURL = base.ResolveReference(audioRef).String()
		}
	}

	return &Playlist{
		PlaylistURL:      resolvedURL,
		RootURL:          rootURL,
		Resolution:       finalResolution,
		Framerate:        finalFramerate,
		AudioPlaylistURL: audioPlaylistURL,
	}, nil
}

type WatchHandler func(b []byte, duration float64) error

// WatchSegments continuously fetches and processes video segments.
func (p *Playlist) WatchSegments(ctx context.Context, handler WatchHandler) error {
	var (
		client   = internal.NewReq()
		lastSeq  = -1
		seenURIs = make(map[string]bool)
	)
	base, err := url.Parse(p.PlaylistURL)
	if err != nil {
		return fmt.Errorf("parse playlist URL: %w", err)
	}
	resolveURL := func(uri string) string {
		ref, err := url.Parse(uri)
		if err != nil {
			return uri
		}
		return base.ResolveReference(ref).String()
	}

	for {
		resp, err := client.Get(ctx, p.PlaylistURL)
		if err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
		if err != nil {
			return fmt.Errorf("decode from: %w", err)
		}
		playlist, ok := pl.(*m3u8.MediaPlaylist)
		if !ok {
			return fmt.Errorf("cast to media playlist")
		}

		if playlist.Map != nil && playlist.Map.URI != "" {
			p.fetchInitSegment(ctx, client, base, playlist.Map.URI)
		}

		for _, v := range playlist.Segments {
			if v == nil {
				continue
			}
			if v.Map != nil && v.Map.URI != "" {
				p.fetchInitSegment(ctx, client, base, v.Map.URI)
			}

			seq := internal.SegmentSeq(v.URI)
			if seq == -1 {
				if seenURIs[v.URI] {
					continue
				}
				seenURIs[v.URI] = true
			} else {
				if seq <= lastSeq {
					continue
				}
				lastSeq = seq
			}

			segURL := resolveURL(v.URI)
			pipeline := func() ([]byte, error) {
				return client.GetBytes(ctx, segURL)
			}
			segData, err := retry.DoWithData(pipeline, retry.Context(ctx), retry.Attempts(3), retry.Delay(600*time.Millisecond), retry.DelayType(retry.FixedDelay))
			if err != nil {
				break
			}
			if err := handler(segData, v.Duration); err != nil {
				return fmt.Errorf("handler: %w", err)
			}
		}
		<-time.After(1 * time.Second)
	}
}

// WatchAudioSegments polls the audio rendition playlist and calls handler for each audio segment.
// Returns nil immediately if no audio playlist is available (non-LLHLS streams).
func (p *Playlist) WatchAudioSegments(ctx context.Context, handler WatchHandler) error {
	if p.AudioPlaylistURL == "" {
		return nil
	}
	var (
		client   = internal.NewReq()
		lastSeq  = -1
		seenURIs = make(map[string]bool)
	)
	base, err := url.Parse(p.AudioPlaylistURL)
	if err != nil {
		return fmt.Errorf("parse audio playlist URL: %w", err)
	}
	resolveURL := func(uri string) string {
		ref, err := url.Parse(uri)
		if err != nil {
			return uri
		}
		return base.ResolveReference(ref).String()
	}

	for {
		resp, err := client.Get(ctx, p.AudioPlaylistURL)
		if err != nil {
			return fmt.Errorf("get audio playlist: %w", err)
		}
		pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
		if err != nil {
			return fmt.Errorf("decode audio playlist: %w", err)
		}
		playlist, ok := pl.(*m3u8.MediaPlaylist)
		if !ok {
			return fmt.Errorf("cast audio playlist")
		}

		if playlist.Map != nil && playlist.Map.URI != "" {
			p.fetchAudioInitSegment(ctx, client, base, playlist.Map.URI)
		}

		for _, v := range playlist.Segments {
			if v == nil {
				continue
			}
			if v.Map != nil && v.Map.URI != "" {
				p.fetchAudioInitSegment(ctx, client, base, v.Map.URI)
			}

			seq := internal.SegmentSeq(v.URI)
			if seq == -1 {
				if seenURIs[v.URI] {
					continue
				}
				seenURIs[v.URI] = true
			} else {
				if seq <= lastSeq {
					continue
				}
				lastSeq = seq
			}

			segURL := resolveURL(v.URI)
			pipeline := func() ([]byte, error) {
				return client.GetBytes(ctx, segURL)
			}
			segData, err := retry.DoWithData(pipeline, retry.Context(ctx), retry.Attempts(3), retry.Delay(600*time.Millisecond), retry.DelayType(retry.FixedDelay))
			if err != nil {
				break
			}
			if err := handler(segData, v.Duration); err != nil {
				return fmt.Errorf("audio handler: %w", err)
			}
		}
		<-time.After(1 * time.Second)
	}
}

func (p *Playlist) fetchInitSegment(ctx context.Context, client *internal.Req, base *url.URL, mapURI string) {
	if mapURI == p.lastInitURI {
		return
	}
	ref, err := url.Parse(mapURI)
	if err != nil {
		return
	}
	data, err := client.GetBytes(ctx, base.ResolveReference(ref).String())
	if err != nil {
		return
	}
	if len(data) < 8 || (string(data[4:8]) != "ftyp" && string(data[4:8]) != "moov") {
		return
	}
	p.InitSegmentData = data
	p.lastInitURI = mapURI
}

func (p *Playlist) fetchAudioInitSegment(ctx context.Context, client *internal.Req, base *url.URL, mapURI string) {
	if mapURI == p.audioLastInitURI {
		return
	}
	ref, err := url.Parse(mapURI)
	if err != nil {
		return
	}
	data, err := client.GetBytes(ctx, base.ResolveReference(ref).String())
	if err != nil {
		return
	}
	if len(data) < 8 || (string(data[4:8]) != "ftyp" && string(data[4:8]) != "moov") {
		return
	}
	p.AudioInitSegmentData = data
	p.audioLastInitURI = mapURI
}
