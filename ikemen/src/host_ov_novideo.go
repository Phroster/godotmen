//go:build overlay

package main

// Host overlay: the overlay build has no stage videos (Godot draws the fight), so FFmpeg and its DLLs stay
// out of ikemen.dll. Same surface as video_ffmpeg.go (built with !overlay); Open always fails, so no BG ever plays one.

import "errors"

type bgVideo struct {
	texture   Texture
	scaleMode BgVideoScaleMode
	muted     bool
}

func (bgv *bgVideo) Open(filename string, volume int, sm BgVideoScaleMode, sf BgVideoScaleFilter, loop bool) error {
	return errors.New("stage videos are not part of the overlay build: " + filename)
}

func (bgv *bgVideo) Tick() error        { return nil }
func (bgv *bgVideo) SetPlaying(on bool) {}
func (bgv *bgVideo) SetVisible(on bool) {}
func (bgv *bgVideo) Reset()             {}
func (bgv *bgVideo) updateAudioVolume() {}
func (bgv *bgVideo) MixerCleared()      {}
func (bgv *bgVideo) Close()             {}
