package channel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/teacat/chaturbate-dvr/chaturbate"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

func (ch *Channel) Monitor() {
	client := chaturbate.NewClient()
	ch.Info("starting to record `%s`", ch.Config.Username)

	ctx, _ := ch.WithCancel(context.Background())

	var err error
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		pipeline := func() error {
			return ch.RecordStream(ctx, client)
		}
		onRetry := func(_ uint, err error) {
			ch.UpdateOnlineStatus(false)
			if errors.Is(err, internal.ErrChannelOffline) || errors.Is(err, internal.ErrPrivateStream) {
				ch.Info("channel is offline or private, try again in %d min(s)", server.Config.Interval)
			} else if errors.Is(err, internal.ErrCloudflareBlocked) {
				ch.Info("channel was blocked by Cloudflare; try with `-cookies` and `-user-agent`? try again in %d min(s)", server.Config.Interval)
			} else if errors.Is(err, context.Canceled) {
				// ...
			} else {
				ch.Error("on retry: %s: retrying in %d min(s)", err.Error(), server.Config.Interval)
			}
		}
		if err = retry.Do(
			pipeline,
			retry.Context(ctx),
			retry.Attempts(0),
			retry.Delay(time.Duration(server.Config.Interval)*time.Minute),
			retry.DelayType(retry.FixedDelay),
			retry.OnRetry(onRetry),
		); err != nil {
			break
		}
	}

	if err := ch.Cleanup(); err != nil {
		ch.Error("cleanup on monitor exit: %s", err.Error())
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		ch.Error("record stream: %s", err.Error())
	}
}

func (ch *Channel) Update() {
	ch.UpdateCh <- true
}

func (ch *Channel) RecordStream(ctx context.Context, client *chaturbate.Client) error {
	stream, err := client.GetStream(ctx, ch.Config.Username)
	if err != nil {
		return fmt.Errorf("get stream: %w", err)
	}
	ch.StreamedAt = time.Now().Unix()
	ch.Sequence = 0

	playlist, err := stream.GetPlaylist(ctx, ch.Config.Resolution, ch.Config.Framerate)
	if err != nil {
		return fmt.Errorf("get playlist: %w", err)
	}

	ch.Playlist = playlist

	if err := ch.NextFile(); err != nil {
		return fmt.Errorf("next file: %w", err)
	}

	defer func() {
		if err := ch.Cleanup(); err != nil {
			ch.Error("cleanup on record stream exit: %s", err.Error())
		}
	}()

	ch.UpdateOnlineStatus(true)
	ch.Info("stream quality - resolution %dp (target: %dp), framerate %dfps (target: %dfps)", playlist.Resolution, ch.Config.Resolution, playlist.Framerate, ch.Config.Framerate)

	// Launch audio recording goroutine if this stream has an audio rendition
	if playlist.AudioPlaylistURL != "" {
		ch.Info("audio rendition found, recording audio separately")
		go func() {
			if err := playlist.WatchAudioSegments(ctx, ch.HandleAudioSegment); err != nil {
				if !errors.Is(err, context.Canceled) {
					ch.Error("audio watch: %s", err.Error())
				}
			}
		}()
	}

	return playlist.WatchSegments(ctx, ch.HandleSegment)
}

// HandleSegment writes a video segment. Prepends the moov init segment on new files.
func (ch *Channel) HandleSegment(b []byte, duration float64) error {
	if ch.Config.IsPaused {
		return retry.Unrecoverable(internal.ErrPaused)
	}

	if ch.Filesize == 0 && ch.Playlist != nil && len(ch.Playlist.InitSegmentData) > 0 {
		n, err := ch.File.Write(ch.Playlist.InitSegmentData)
		if err != nil {
			return fmt.Errorf("write video init segment: %w", err)
		}
		ch.Filesize += n
	}

	n, err := ch.File.Write(b)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	ch.Filesize += n
	ch.Duration += duration
	ch.Info("duration: %s, filesize: %s", internal.FormatDuration(ch.Duration), internal.FormatFilesize(ch.Filesize))
	ch.Update()

	if ch.ShouldSwitchFile() {
		if err := ch.NextFile(); err != nil {
			return fmt.Errorf("next file: %w", err)
		}
		ch.Info("max filesize or duration exceeded, new file created: %s", ch.File.Name())
	}
	return nil
}

// HandleAudioSegment writes an audio segment to the separate audio file.
func (ch *Channel) HandleAudioSegment(b []byte, duration float64) error {
	if ch.AudioFile == nil {
		return nil
	}

	if ch.AudioFilesize == 0 && ch.Playlist != nil && len(ch.Playlist.AudioInitSegmentData) > 0 {
		n, err := ch.AudioFile.Write(ch.Playlist.AudioInitSegmentData)
		if err != nil {
			return fmt.Errorf("write audio init segment: %w", err)
		}
		ch.AudioFilesize += n
	}

	n, err := ch.AudioFile.Write(b)
	if err != nil {
		return fmt.Errorf("write audio file: %w", err)
	}
	ch.AudioFilesize += n
	return nil
}
