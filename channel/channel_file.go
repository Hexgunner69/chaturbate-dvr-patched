package channel

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Pattern struct {
	Username string
	Year     string
	Month    string
	Day      string
	Hour     string
	Minute   string
	Second   string
	Sequence int
}

func (ch *Channel) NextFile() error {
	if err := ch.Cleanup(); err != nil {
		return err
	}
	filename, err := ch.GenerateFilename()
	if err != nil {
		return err
	}
	if err := ch.CreateNewFile(filename); err != nil {
		return err
	}
	ch.Sequence++
	return nil
}

// Cleanup closes the current file(s), deletes short clips, and remuxes to .mp4.
// If both video and audio files exist, muxes them together. Otherwise remuxes video-only.
func (ch *Channel) Cleanup() error {
	if ch.File == nil {
		return nil
	}
	videoName := ch.File.Name()
	currentDuration := ch.Duration

	defer func() {
		ch.Filesize = 0
		ch.Duration = 0
		ch.AudioFilesize = 0
	}()

	if err := ch.File.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("sync file: %w", err)
	}
	if err := ch.File.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("close file: %w", err)
	}

	// Close audio file if present
	var audioName string
	if ch.AudioFile != nil {
		audioName = ch.AudioFile.Name()
		ch.AudioFile.Sync()
		ch.AudioFile.Close()
		ch.AudioFile = nil
	}

	fileInfo, err := os.Stat(videoName)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat file: %w", err)
	}

	// Delete zero-byte files and short clips (CDN bootstrap artifacts < 30s)
	const minDuration = 30.0
	if fileInfo != nil && (fileInfo.Size() == 0 || currentDuration < minDuration) {
		os.Remove(videoName)
		if audioName != "" {
			os.Remove(audioName)
		}
		return nil
	}

	if strings.HasSuffix(videoName, ".ts") {
		mp4 := strings.TrimSuffix(videoName, ".ts") + ".mp4"
		var cmd *exec.Cmd
		if audioName != "" {
			// Mux video + audio into mp4
			cmd = exec.Command("ffmpeg", "-y", "-loglevel", "error",
				"-i", videoName, "-i", audioName,
				"-c", "copy", "-shortest", mp4)
		} else {
			// Video-only remux
			cmd = exec.Command("ffmpeg", "-y", "-loglevel", "error",
				"-i", videoName, "-c", "copy", mp4)
		}
		if err := cmd.Run(); err == nil {
			os.Remove(videoName)
			if audioName != "" {
				os.Remove(audioName)
			}
		} else if audioName != "" {
			// Mux failed — try video-only fallback
			cmd2 := exec.Command("ffmpeg", "-y", "-loglevel", "error",
				"-i", videoName, "-c", "copy", mp4)
			if cmd2.Run() == nil {
				os.Remove(videoName)
			}
			os.Remove(audioName)
		}
	}
	return nil
}

func (ch *Channel) GenerateFilename() (string, error) {
	var buf bytes.Buffer
	tpl, err := template.New("filename").Parse(ch.Config.Pattern)
	if err != nil {
		return "", fmt.Errorf("filename pattern error: %w", err)
	}
	t := time.Unix(ch.StreamedAt, 0)
	pattern := &Pattern{
		Username: ch.Config.Username,
		Sequence: ch.Sequence,
		Year:     t.Format("2006"),
		Month:    t.Format("01"),
		Day:      t.Format("02"),
		Hour:     t.Format("15"),
		Minute:   t.Format("04"),
		Second:   t.Format("05"),
	}
	if err := tpl.Execute(&buf, pattern); err != nil {
		return "", fmt.Errorf("template execution error: %w", err)
	}
	return buf.String(), nil
}

func (ch *Channel) CreateNewFile(filename string) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0777); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}
	file, err := os.OpenFile(filename+".ts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
	if err != nil {
		return fmt.Errorf("cannot open file: %s: %w", filename, err)
	}
	ch.File = file

	// Only create audio file if this channel has an audio stream
	if ch.Playlist != nil && ch.Playlist.AudioPlaylistURL != "" {
		audioFile, err := os.OpenFile(filename+"_audio.ts", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0777)
		if err == nil {
			ch.AudioFile = audioFile
		}
	}

	return nil
}

func (ch *Channel) ShouldSwitchFile() bool {
	maxFilesizeBytes := ch.Config.MaxFilesize * 1024 * 1024
	maxDurationSeconds := ch.Config.MaxDuration * 60

	return (ch.Duration >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
		(ch.Filesize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}
