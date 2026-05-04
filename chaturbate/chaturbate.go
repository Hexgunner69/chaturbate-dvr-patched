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
	return &Client{
		Req: internal.NewReq(),
	}
}

func (c *Client) GetStream(ctx context.Context, username string) (*Stream, error) {
	return FetchStream(ctx, c.Req, username)
}

func FetchStream(ctx context.Context, client *internal.Req, username string) (*Stream, error) {
	body, err := client.Get(ctx, fmt.Sprintf("%s%s", server.Config.Domain, username))
	if err != nil {
		return nil, fmt.Errorf("failed to get page body: %w", err)
	}

	// Support both regular HLS (playlist.m3u8) and Low-Latency HLS (llhls.m3u8)
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

	return PickPlaylist(masterPlaylist, hlsSource, resolution, framerate)
}

// Playlist represents an HLS playlist containing variant streams.
type Playlist struct {
	PlaylistURL     string
	RootURL         string
	Resolution      int
	Framerate       int
	InitSegmentData []byte // EXT-X-MAP moov box; written at the start of each new file
	lastInitURI     string
}

type Resolution struct {
	Framerate map[int]string
	Width     int
}

// PickPlaylist selects the best matching variant stream.
// Uses net/url to correctly handle both relative and absolute-path variant URIs (LLHLS).
func PickPlaylist(masterPlaylist *m3u8.MasterPlaylist, baseURL string, resolution, framerate int) (*Playlist, error) {
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

	// Resolve variant URI against base URL — handles relative paths and absolute paths like /v1/edge/...
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	variantRef, err := url.Parse(playlistURI)
	if err != nil {
		return nil, fmt.Errorf("parse variant URL: %w", err)
	}
	resolvedURL := base.ResolveReference(variantRef).String()

	// RootURL = directory containing the playlist (used to resolve relative segment URIs)
	lastSlash := strings.LastIndex(resolvedURL, "/")
	rootURL := resolvedURL[:lastSlash+1]

	return &Playlist{
		PlaylistURL: resolvedURL,
		RootURL:     rootURL,
		Resolution:  finalResolution,
		Framerate:   finalFramerate,
	}, nil
}

type WatchHandler func(b []byte, duration float64) error

// WatchSegments continuously fetches and processes video segments.
// For LLHLS streams: uses URI-based dedup and fetches the EXT-X-MAP init segment (moov box).
// Init data is stored in p.InitSegmentData; HandleSegment writes it at the start of each file.
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

		// Fetch playlist-level EXT-X-MAP init segment
		if playlist.Map != nil && playlist.Map.URI != "" {
			p.fetchInitSegment(ctx, client, base, playlist.Map.URI)
		}

		for _, v := range playlist.Segments {
			if v == nil {
				continue
			}

			// Fetch segment-level EXT-X-MAP (overrides playlist-level)
			if v.Map != nil && v.Map.URI != "" {
				p.fetchInitSegment(ctx, client, base, v.Map.URI)
			}

			// LLHLS URIs end in _llhls.ts so SegmentSeq returns -1; use URI dedup instead
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

			segData, err := retry.DoWithData(
				pipeline,
				retry.Context(ctx),
				retry.Attempts(3),
				retry.Delay(600*time.Millisecond),
				retry.DelayType(retry.FixedDelay),
			)
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

// fetchInitSegment downloads the EXT-X-MAP initialization segment and stores it.
// Only re-fetches when the URI changes. Validates the response is real MP4 data, not an error page.
func (p *Playlist) fetchInitSegment(ctx context.Context, client *internal.Req, base *url.URL, mapURI string) {
	if mapURI == p.lastInitURI {
		return
	}
	ref, err := url.Parse(mapURI)
	if err != nil {
		return
	}
	initURL := base.ResolveReference(ref).String()

	data, err := client.GetBytes(ctx, initURL)
	if err != nil {
		return
	}
	// Must be a valid ISO BMFF box (ftyp or moov) — reject error pages
	if len(data) < 8 || (string(data[4:8]) != "ftyp" && string(data[4:8]) != "moov") {
		return
	}
	p.InitSegmentData = data
	p.lastInitURI = mapURI
}
