// Copyright 2021 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package readerdriver

import (
	"io"
	"runtime"
	"sync"

	"github.com/hajimehoshi/ebiten/v2/audio/internal/oboe"
)

func IsAvailable() bool {
	return true
}

type context struct {
	sampleRate      int
	channelNum      int
	bitDepthInBytes int
}

func NewContext(sampleRate int, channelNum int, bitDepthInBytes int) (Context, chan struct{}, error) {
	ready := make(chan struct{})
	close(ready)

	c := &context{
		sampleRate:      sampleRate,
		channelNum:      channelNum,
		bitDepthInBytes: bitDepthInBytes,
	}
	return c, ready, nil
}

func (c *context) NewPlayer(src io.Reader) Player {
	p := &player{
		context: c,
		src:     src,
		cond:    sync.NewCond(&sync.Mutex{}),
		volume:  1,
	}
	runtime.SetFinalizer(p, (*player).Close)
	return p
}

func (c *context) Suspend() error {
	return oboe.Suspend()
}

func (c *context) Resume() error {
	return oboe.Resume()
}

func (c *context) Close() error {
	// TODO: Implement this
	return nil
}

type player struct {
	context *context
	p       *oboe.Player
	src     io.Reader
	err     error
	cond    *sync.Cond
	state   playerState
	volume  float64
}

func (p *player) Pause() {
	p.Reset()
}

func (p *player) Play() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.err != nil {
		return
	}
	if p.state != playerPaused {
		return
	}
	defer p.cond.Signal()
	if p.p == nil {
		p.p = oboe.NewPlayer(p.context.sampleRate, p.context.channelNum, p.context.bitDepthInBytes, p.volume, func() {
			p.cond.Signal()
		})
		go p.loop()
	}
	if err := p.p.Play(); err != nil {
		p.setErrorImpl(err)
		return
	}
	p.state = playerPlay
}

func (p *player) IsPlaying() bool {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.state == playerPlay
}

func (p *player) Reset() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.err != nil {
		return
	}
	if p.state == playerClosed {
		return
	}
	if p.p == nil {
		return
	}
	defer func() {
		p.p = nil
		p.cond.Signal()
	}()
	if err := p.p.Close(); err != nil {
		p.setErrorImpl(err)
		return
	}
	p.state = playerPaused
}

func (p *player) Volume() float64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.volume
}

func (p *player) SetVolume(volume float64) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.volume = volume
	if p.p == nil {
		return
	}
	p.p.SetVolume(volume)
}

func (p *player) UnplayedBufferSize() int64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	if p.p == nil {
		return 0
	}
	return p.p.UnplayedBufferSize()
}

func (p *player) Err() error {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.err
}

func (p *player) Close() error {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.closeImpl()
}

func (p *player) closeImpl() error {
	defer p.cond.Signal()

	runtime.SetFinalizer(p, nil)
	p.state = playerClosed
	if p.p == nil {
		return p.err
	}
	if err := p.p.Close(); err != nil && p.err == nil {
		p.setErrorImpl(err)
		return p.err
	}
	p.p = nil
	return p.err
}

func (p *player) setError(err error) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.setErrorImpl(err)
}

func (p *player) setErrorImpl(err error) {
	p.err = err
	p.closeImpl()
}

func (p *player) shouldWait() bool {
	if p.p == nil {
		return false
	}
	switch p.state {
	case playerPlay:
		return p.p.UnplayedBufferSize() >= int64(p.context.MaxBufferSize())
	case playerPaused:
		return true
	case playerClosed:
		return false
	default:
		panic("not reached")
	}
}

func (p *player) wait() bool {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	for p.shouldWait() {
		p.cond.Wait()
	}
	return p.p != nil && p.state == playerPlay
}

func (p *player) write(buf []byte) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.state == playerClosed {
		return
	}
	if p.p == nil {
		return
	}
	p.p.AppendBuffer(buf)
}

func (p *player) loop() {
	buf := make([]byte, 4096)
	for {
		if !p.wait() {
			return
		}

		n, err := p.src.Read(buf)
		if err != nil && err != io.EOF {
			p.setError(err)
			return
		}
		p.write(buf[:n])

		// Now p.p.Reset() doesn't close the stream gracefully. Then buffer size check is necessary here.
		if err == io.EOF && p.UnplayedBufferSize() == 0 {
			p.Reset()
			return
		}
	}
}
