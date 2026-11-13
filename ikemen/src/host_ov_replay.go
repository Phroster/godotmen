package main

// The replay reel: clips of the live match the host can show again. These are immutable copies of Ikemen's cued draw
// lists and started sounds, not state re-simulation, converted sprites, or a second renderer. The host opens a clip
// on a fighter (and, optionally, the fighter that struck it), scores and closes it, and owns the camera a clip is
// shown with. All calls happen while the match thread is parked; sound reads additionally take the mixer lock.

import (
	"encoding/json"
	"fmt"
	"hash/maphash"
	"math"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const ovReplayPreRoll = 120
const ovReplayMaxTicks = 3600

type ovReplayAssetKey struct {
	source *Sprite
	pal    uint64 // maphash of the resolved colours; a hit is verified colour for colour (ovReplayAssetFor)
	n      int    // (a second palette with the same hash: the next index)
}

var ovReplaySeed = maphash.MakeSeed()

type ovReplayAsset struct {
	key     ovReplayAssetKey
	sprite  Sprite
	palette PaletteList
	refs    int
	bytes   int64    // charged now
	pixels  int64    // the source sprite's texels, charged once their owner leaves the match (ovReplayOwnerGone)
	owner   int32    // the root fighter whose own SFF holds those texels (-1: charged at once)
	srcPal  []uint32 // the resolved palette it was frozen from (its identity: see ovReplayAssetFor's hint)
}

// A frozen sprite's look: its animation (the fields Animation.Draw and DrawList.draw read; the
// animation clock zeroed), PalFX and frame, shared by consecutive ticks that draw it unchanged - an animation element
// lasts several ticks and a walking fighter only changes its SpriteData's position - instead of a full copy per entry
// per tick (~1000 B an entry: ~1 MB a tick with a hundred fighters filled the budget with a few seconds of clips).
type ovReplayLook struct {
	anim   Animation
	pfx    PalFX
	frame  [1]AnimFrame
	asset  *ovReplayAsset
	hasPfx bool
	refs   int // entries (in every retained frame) drawing this look
}

const ovReplayLookBytes = int64(unsafe.Sizeof(ovReplayLook{})) + 16

// One entry of a frame's draw list: shared by consecutive frames that cue it unchanged - a body lying still, an
// effect at rest, a fighter between two steps of its animation - so a frame holds pointers, not copies. Its sortindex
// and syncGroup are the draw's own (set by ovReplayRenderState and DrawList.draw while a clip shows; 0 when recorded).
//
// Its pointers come first - its look, and its custom shader when it has one - and its SpriteData as a pointer-free
// image after them (ovReplaySD), so the GC's mark reads 16 bytes of an entry instead of ~360: with millions of
// entries they would be half of the heap the mark phase scans. sd() views the image; ovReplayUnpack gives the whole
// SpriteData back - its animation and PalFX its look's, as recorded.
type ovReplaySprite struct {
	look     *ovReplayLook           // nil: a blank sort-only entry (a sync group's member that is not drawn)
	shader   *CustomShaderRenderData // its custom shader, when any of it points anywhere (the image keeps no pointer)
	data     ovReplaySD              // the SpriteData as recorded, every pointer word zero
	identity uintptr                 // identity only: must not retain an animation's entire SFF
	refs     int                     // frames holding it
	usedAt   uint64                  // the newest frame it was placed in (an entry stands once in a frame)
	owner    int32                   // its root fighter's Char id + 1 (0: none): a clip's activity
}

// (the entry's layout before the GC pass, for the budget's accounting only: the bytes an entry charges - and so which
// clips the budget drops, when - stay exactly as they were)
type ovReplaySpriteSized struct {
	data     SpriteData
	look     *ovReplayLook
	identity uintptr
	refs     int
	usedAt   uint64
}

const ovReplayEntryBytes = int64(unsafe.Sizeof(ovReplaySpriteSized{})) + 16

// ovReplayNewEntry: storage for one entry. Entries live in slabs of ovReplaySlabN, so a million live entries are a
// thousand heap objects for a GC mark to visit, not a million (the frames' interior pointers keep a slab alive). A
// released slot comes back through ovReplayFreeEntry.
const ovReplaySlabN = 1024

var ovReplaySlab []ovReplaySprite // the rest of the newest slab

// Released slots (their count passed exactly 0: no frame holds them; their look and custom shader already cleared where
// they were released) wait as whole lists - the parallel drop's per-worker lists as they are, no copy, no touch of the
// slot - until two capture boundaries (ovReplaySlotsBoundary, at the identity maps' swap) have passed: an entry released
// before a boundary can still be named by the newest frame's identity maps after it (where a lookup took a released
// entry for none - the newest frame itself dropped), never after the next one. Then they are handed out again.
var ovReplayFreeing, ovReplayFreeingOld, ovReplayFreeLists [][]*ovReplaySprite
var ovReplayFreeingCur []*ovReplaySprite // (the serial releases' list, collecting)

func ovReplayNewEntry() *ovReplaySprite {
	for n := len(ovReplayFreeLists); n > 0; n = len(ovReplayFreeLists) {
		l := ovReplayFreeLists[n-1]
		if len(l) == 0 {
			ovReplayFreeLists[n-1] = nil
			ovReplayFreeLists = ovReplayFreeLists[:n-1]
			continue
		}
		e := l[len(l)-1]
		l[len(l)-1] = nil
		ovReplayFreeLists[n-1] = l[:len(l)-1]
		return e
	}
	if len(ovReplaySlab) == 0 {
		ovReplaySlab = make([]ovReplaySprite, ovReplaySlabN)
	}
	e := &ovReplaySlab[0]
	ovReplaySlab = ovReplaySlab[1:]
	return e
}

// ovReplayFreeEntry: (the recorder's thread; e released, its look cleared) its slot waits for reuse. A count driven
// below 0 (never seen) keeps its slot for good rather than hand it out twice.
func ovReplayFreeEntry(e *ovReplaySprite) {
	if e.refs != 0 {
		return
	}
	e.shader = nil
	if len(ovReplayFreeingCur) == cap(ovReplayFreeingCur) && len(ovReplayFreeingCur) > 0 {
		ovReplayFreeing = append(ovReplayFreeing, ovReplayFreeingCur)
		ovReplayFreeingCur = make([]*ovReplaySprite, 0, 4096)
	}
	ovReplayFreeingCur = append(ovReplayFreeingCur, e)
}

// ovReplaySlotsBoundary: (ovReplayCapture, at the identity maps' swap) the slots released before the previous boundary
// become free; those released since wait for the next one
func ovReplaySlotsBoundary() {
	ovReplayFreeLists = append(ovReplayFreeLists, ovReplayFreeingOld...)
	if len(ovReplayFreeingCur) > 0 {
		ovReplayFreeing = append(ovReplayFreeing, ovReplayFreeingCur)
		ovReplayFreeingCur = nil
	}
	ovReplayFreeingOld, ovReplayFreeing = ovReplayFreeing, ovReplayFreeingOld[:0]
	for i := range ovReplayFreeing[:cap(ovReplayFreeing)] { // (the reused list of lists keeps no stale list)
		ovReplayFreeing[:cap(ovReplayFreeing)][i] = nil
	}
}

// ovReplaySD: a SpriteData's bytes with every pointer-holding field zeroed (ovReplaySDPtrs: its animation, PalFX,
// custom shader name and textures, ovRoot, ovExplod) - a type without pointers, which the GC never scans. sd() views it
// as a SpriteData for its value fields; a pointer field must never be written through that view (the GC would not see
// the pointer), only through ovReplayPack.
type ovReplaySD struct {
	w [unsafe.Sizeof(SpriteData{}) / 8]uint64
}

// the byte ranges of SpriteData's pointer-holding fields, from its type (anything added to SpriteData is covered)
var ovReplaySDPtrs = ovPtrRanges(reflect.TypeOf(SpriteData{}), 0, nil)

func ovPtrRanges(t reflect.Type, off uintptr, out [][2]uintptr) [][2]uintptr {
	switch t.Kind() {
	case reflect.Ptr, reflect.UnsafePointer, reflect.String, reflect.Slice, reflect.Map, reflect.Interface,
		reflect.Func, reflect.Chan:
		return append(out, [2]uintptr{off, off + t.Size()})
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			out = ovPtrRanges(f.Type, off+f.Offset, out)
		}
	case reflect.Array:
		for i := 0; i < t.Len(); i++ {
			out = ovPtrRanges(t.Elem(), off+uintptr(i)*t.Elem().Size(), out)
		}
	}
	return out
}

// sd: the entry's SpriteData image, viewed as one (its pointer fields read nil; write value fields only)
func (e *ovReplaySprite) sd() *SpriteData { return (*SpriteData)(unsafe.Pointer(&e.data)) }

// ovReplayPack: sd as e's record - its bytes, every pointer word zeroed, the order/sync fields cleared (the draw's
// own); a custom shader that points anywhere kept apart, where the GC sees it
func ovReplayPack(e *ovReplaySprite, sd *SpriteData) {
	n := unsafe.Sizeof(*sd)
	img := unsafe.Slice((*byte)(unsafe.Pointer(&e.data)), n)
	copy(img, unsafe.Slice((*byte)(unsafe.Pointer(sd)), n))
	for _, r := range ovReplaySDPtrs {
		for i := r[0]; i < r[1]; i++ {
			img[i] = 0
		}
	}
	v := e.sd()
	v.sortindex, v.syncGroup = 0, 0
	e.shader = nil
	if cs := &sd.customShader; cs.name != "" || cs.tex1 != nil || cs.tex2 != nil {
		c := *cs
		e.shader = &c
	}
}

// ovReplayUnpack: e's SpriteData as recorded - the image, its animation and PalFX its look's (nil for a blank), its
// custom shader
func ovReplayUnpack(e *ovReplaySprite, dst *SpriteData) {
	*dst = *e.sd()
	if e.look != nil {
		dst.anim = &e.look.anim
		if e.look.hasPfx {
			dst.pfx = &e.look.pfx
		}
	}
	if e.shader != nil {
		dst.customShader = *e.shader
	}
}

// ovReplaySameData: a's record equals b's - the images, and the custom shaders (the bytes the whole SpriteData's
// comparison compared; the animation and PalFX are the look's, compared by the callers)
func ovReplaySameData(a, b *ovReplaySprite) bool {
	if !ovBytesEqual(&a.data, &b.data) {
		return false
	}
	if a.shader == nil || b.shader == nil {
		return a.shader == nil && b.shader == nil
	}
	return ovBytesEqual(a.shader, b.shader)
}

// A recorded sound's original sample: charged to the budget once no live root's own SND holds it - until then the
// fighter's SND keeps those bytes anyway (as its SFF keeps the texels of its frozen sprites)
type ovReplaySample struct {
	refs    int
	charged bool
	owner   int32 // the root (Char id) whose own SND holds it; -1: charged
}

type ovReplaySound struct {
	id                           uint64
	sound                        *Sound // original sample, kept alive even after its owner despawns
	group, number, channel, loop int32
	freq, volume                 float32
	pan                          float32
	gains                        [2]float32
	loopStart, loopEnd, start    int
	position, remaining          int
	play, stop, paused           bool
}

type ovReplayVoice struct {
	channel *SoundChannel
	request ovReplaySound
}

type ovReplayFrame struct {
	tick    uint64
	refs    int
	bytes   int64
	pruned  bool // cut to its clips' view when it left the pre-roll history (ovReplayPrune)
	sprites []*ovReplaySprite
	listCap int // (the draw list is made when the frame is shown - ovReplayDrawList; the capacity
	// the list it kept had is still charged, so the budget's accounting is as before)
	cam             Camera
	shake           EnvShake
	allFX           PalFX
	superTime       int32
	superBrightness float32
	gameTick        int
	frameCounter    int32
	sounds          []ovReplaySound
	poses           map[int]ovReplayPose
}

// A root's floor x/depth/height (host coordinates, the state record's r[2..4]) and whether its own body was actually
// cued; id tells a reused slot's next fighter apart
type ovReplayPose struct {
	at [4]float32
	id int32
}

type ovReplayClip struct {
	id       int
	frames   []*ovReplayFrame
	open     bool
	score    float64
	seen     []bool
	victim   int
	victimID int32 // the victim's Char id at clip_open (-1: none was there)
	killer   int   // the killer's handle (clip_score/clip_close "k", else the victim's last attacker); -1
	killerID int32
	viewAt   [2][2]float32 // the victim's and killer's last known ik x/z (ovReplayPrune walks the frames in tick order)
	viewOK   [2]bool
}

type ovReplayRecorder struct {
	enabled             bool
	budget, bytes, peak int64
	ring                []*ovReplayFrame
	clips               map[int]*ovReplayClip
	assets              map[ovReplayAssetKey]*ovReplayAsset
	samples             map[*Sound]*ovReplaySample
	ownedSamples        map[int32]map[*Sound]bool // samples a live root's own SND holds
	voices              map[*SoundEffect]*ovReplayVoice
	stops               []ovReplaySound
	serial              uint64
	playing             *ovReplayClip
	shown               int
	between             float32
	begun               bool // the playing clip has shown a frame: a shot cut in
	// later than its first frame resumes the sounds already playing there on the first frame it shows, as tick 0 did
	channels                              map[uint64]*SoundChannel
	livePaused                            map[*SoundChannel]bool
	captureUS, captureMaxUS, captures     int64
	audioPlays, audioStops, repeatedShows int64

	// live, and such sounds recorded
	dropped                               int
	assetBytes, sampleBytes               int64                             // (of bytes: frozen sprites/palettes, WAVs - replay_stats)
	owned                                 map[int32]map[*ovReplayAsset]bool // assets whose texels a live root's SFF holds
	unwarmed                              []*ovReplayAsset                  // assets ovReplayWarm has not readied yet
	prevEntries, nextEntries              map[uintptr]*ovReplaySprite       // the newest frame's entries by identity
	lookBytes, looksNew, looksShared      int64
	entryBytes, entriesNew, entriesShared int64
	pruneOn                               bool
	pruneKept, pruneTotal, pruned         int64
}

var ovReplay ovReplayRecorder
var ovReplayPan sync.Map // replay *SoundEffect -> exact as-heard left/right gains
var ovReplayAudioWriting bool
var ovReplayAudioStart *ovReplaySound

func ovReplayConfigure(cfg map[string]interface{}) {
	mb := 256.0
	if n, ok := cfg["replay_mb"].(float64); ok {
		mb = math.Max(4, math.Min(n, 1024))
	}
	on, _ := cfg["replay"].(bool)
	if os.Getenv("IKEMEN_REPLAY") == "0" {
		on = false // (switch: nothing is recorded, clips cannot be opened)
	}
	ovReplaySlab, ovReplayFreeing, ovReplayFreeingOld, ovReplayFreeLists, ovReplayFreeingCur = nil, nil, nil, nil, nil
	// (the entries' slabs and free slots go with the old recorder)
	ovReplay = ovReplayRecorder{enabled: on, budget: int64(mb * (1 << 20)),
		clips: make(map[int]*ovReplayClip), assets: make(map[ovReplayAssetKey]*ovReplayAsset),
		samples: make(map[*Sound]*ovReplaySample), voices: make(map[*SoundEffect]*ovReplayVoice),
		ownedSamples: make(map[int32]map[*Sound]bool),
		channels:     make(map[uint64]*SoundChannel), livePaused: make(map[*SoundChannel]bool),
		owned: make(map[int32]map[*ovReplayAsset]bool), prevEntries: make(map[uintptr]*ovReplaySprite),
		nextEntries: make(map[uintptr]*ovReplaySprite)}
	// (init config "replay_prune": true - frames held only by clips are cut to what a camera in front of the clip's
	// fighters can show: ovReplayPrune. Off by default: the host's replay camera may stand anywhere.)
	ovReplay.pruneOn, _ = cfg["replay_prune"].(bool)
	// The recorder's retained storage is Go heap on top of the fighters' (the memory soft limit is sized for those
	// alone). The limit grows by the budget, so the GC keeps the headroom it had.
	if on && !ovReplayLimitRaised {
		ovReplayLimitRaised = true // (once: a retried ikemen_init configures again)
		if limit := debug.SetMemoryLimit(-1); limit > 0 && limit < math.MaxInt64-ovReplay.budget {
			debug.SetMemoryLimit(limit + ovReplay.budget)
			ovLog("memory: replay budget %d MB, Go memory limit %d MB", ovReplay.budget>>20, (limit+ovReplay.budget)>>20)
		}
	}
}

var ovReplayLimitRaised bool

func ovReplayIsPlaying() bool { return ovReplay.playing != nil }

// SoundChannel's two taps are AFTER the sample lookup and the channel priority
// decision. Reset also records StopSnd, state/hit cuts and despawn cuts. Setter
// results and moving-owner pan are sampled at each tick.
func ovReplaySoundPlay(ch *SoundChannel, loop int32, start, end, position int) {
	if !ovOn || !ovReplay.enabled || ovReplayAudioWriting || ch.sfx == nil {
		return
	}
	ovReplay.serial++
	ovReplay.voices[ch.sfx] = &ovReplayVoice{channel: ch, request: ovReplaySound{
		id: ovReplay.serial, sound: ch.sound, group: ch.group, number: ch.number,
		channel: ch.channelNo, loop: loop, loopStart: start, loopEnd: end, start: position, play: true}}
}


func ovReplaySoundStop(ch *SoundChannel) {
	ovReplayPan.Delete(ch.sfx)
	if !ovOn || !ovReplay.enabled || ovReplayAudioWriting || ch.sfx == nil {
		return
	}
	if v := ovReplay.voices[ch.sfx]; v != nil {
		// A request replaced/Stopped within this tick still belongs to the tick's
		// ordered sound log. Sample its final setters before Reset clears them.
		if v.request.play {
			WithSpeakerLock(func() { ovReplay.stops = append(ovReplay.stops, ovReplaySnapshotVoice(v)) })
		}
		ovReplay.stops = append(ovReplay.stops, ovReplaySound{id: v.request.id, stop: true})
		delete(ovReplay.voices, ch.sfx)
	}
}

// SoundChannel.Play calls this with the mixer locked, BEFORE adding the new
// voice. Otherwise an audio callback could hear its default volume/pan in the
// gap before replay's setters run. Ordinary native PlaySnd is unchanged.
func ovReplaySoundPrepare(ch *SoundChannel) {
	if !ovReplayAudioWriting || ovReplayAudioStart == nil {
		return
	}
	r := ovReplayAudioStart
	ch.sfx.volume = r.volume
	ch.ctrl.Paused = r.paused || r.freq == 0
	ovReplayPan.Store(ch.sfx, r.gains)
}

func ovReplayAudioGains(sfx *SoundEffect) (float32, float32, bool) {
	if p, ok := ovReplayPan.Load(sfx); ok {
		g := p.([2]float32)
		return g[0], g[1], true
	}
	return 0, 0, false
}

// Caller holds the native mixer mutex; the owner/camera are parked or on the
// logic thread. Keep native float32 arithmetic identical to SoundEffect.Stream.
func ovReplaySnapshotVoice(v *ovReplayVoice) ovReplaySound {
	ch, r := v.channel, v.request
	r.freq, r.volume, r.paused, r.pan = ch.sfx.freqmul, ch.sfx.volume, ch.ctrl.Paused, ch.sfx.pan
	r.position = ch.streamer.Position()
	if looper, ok := ch.sfx.streamer.(*StreamLooper); ok {
		r.remaining = looper.loopcount
	}
	r.gains = [2]float32{r.volume, r.volume}
	left, right, host := ovAudioPanGains(ch.sfx)
	if !host && sys.cfg.Sound.StereoEffects && (ch.sfx.x != nil || ch.sfx.pan != 0) {
		leftEdge := sys.cam.ScreenPos[0] + sys.cam.Offset[0]
		width := float32(sys.gameWidth) / sys.cam.Scale
		rightEdge := leftEdge + width
		p := ((rightEdge-leftEdge)/2 - ch.sfx.pan) / width
		if ch.sfx.x != nil {
			p = ((rightEdge - ch.sfx.localscl**ch.sfx.x) - ch.sfx.pan) / width
		}
		scale, offset := sys.cfg.Sound.PanningRange/100, (100-sys.cfg.Sound.PanningRange)/200
		left = Clamp(r.volume*2*(p*scale+offset), 0, 512)
		right = Clamp(r.volume*2*((1-p)*scale+offset), 0, 512)
		host = true
	}
	if host {
		r.gains = [2]float32{left, right}
	}
	return r
}

func ovReplaySoundSnapshot() []ovReplaySound {
	var result []ovReplaySound
	WithSpeakerLock(func() {
		result = append(result, ovReplay.stops...)
		ovReplay.stops = nil
		for _, v := range ovReplay.voices {
			ch := v.channel
			if ch.sfx == nil {
				continue
			}
			result = append(result, ovReplaySnapshotVoice(v))
			v.request.play = false
		}
	})
	// Map traversal order must never reorder two plays sharing a channel.
	// IDs follow the actual accepted requests (including channels -1).
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].id < result[j-1].id; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func ovReplayAssetFor(sd *SpriteData, hint *ovReplayAsset) (res *ovReplayAsset) {
	a := sd.anim
	pal := ovReplayPalOf(a, sd.pfx) // CPU only; resolve RemapPal BEFORE freezing its colors
	// The same animation's asset in the newest frame (hint) is the answer when it was frozen from this
	// very palette - the same source sprite and the same resolved palette array. Palette arrays are never written
	// after they are made (PaletteList.SetSource and Sprite.Pal take new slices; RemapPal/PalFX change which array a
	// sprite resolves to, never its colours), and srcPal keeps the array alive, so the same array holds the colours
	// the asset copied; and an asset map holds at most one asset per (source, colours) - the chain below returns it.
	// So the lookup would return hint: skipped are the colour hash, the map search and the 1 KB compare (about
	// 0.35 ms of the join a tick with a hundred fighters). IKEMEN_REPLAY_HINT=0: always the lookup; =verify: both, counted.
	if hint != nil && ovReplayHint != 0 && hint.refs > 0 && hint.key.source == a.spr && len(pal) > 0 &&
		len(hint.srcPal) == len(pal) && &hint.srcPal[0] == &pal[0] {
		if ovReplayHint == 1 {
			return hint
		}
		defer func() {
			atomic.AddInt64(&ovReplayHintChecks, 1)
			if res != hint {
				atomic.AddInt64(&ovReplayHintMisses, 1)
			}
		}()
	}
	// (a 64-bit maphash and an exact compare instead of sha256 - every cued sprite is looked up every
	// tick, ~450 a tick at 32 fighters)
	var h uint64
	if len(pal) > 0 {
		h = maphash.Bytes(ovReplaySeed, unsafe.Slice((*byte)(unsafe.Pointer(&pal[0])), len(pal)*4))
	}
	key := ovReplayAssetKey{source: a.spr, pal: h}
	for ; ; key.n++ {
		asset := ovReplay.assets[key]
		if asset == nil {
			break
		}
		if ovU32Equal(asset.sprite.Pal, pal) {
			return asset
		}
	}
	asset := &ovReplayAsset{key: key, sprite: *a.spr, srcPal: pal}
	asset.sprite.Pal = append([]uint32(nil), pal...)
	// A live CachePalTex mutates its texture in-place. Never retain that palette
	// texture: this immutable sprite gets one of its own, during ahead rendering.
	asset.sprite.PalTex, asset.sprite.paltemp = nil, nil
	asset.sprite.palidx = 0
	asset.sprite.ovSrc = a.spr
	asset.sprite.ovPx, asset.sprite.ovDecode, asset.sprite.ovPixelOwner = nil, nil, nil // (shared texels: ovSrc owns them)
	asset.palette.paletteMap = []int{0}
	asset.palette.palettes = [][]uint32{asset.sprite.Pal}
	asset.palette.PalTex = []Texture{nil}
	// The frozen copy and its own palette (+ its palette texture) are the replay's own. Its texels (the source sprite's
	// GPU texture) are the live fighter's while that fighter is in the match - its SFF holds every sprite it drew
	// anyway - and become the replay's alone once it leaves: charged then (charging every frozen sprite's texels at
	// once would evict clips for bytes that are in memory anyway). Sprites from another SFF (a foreign state's,
	// fightfx's) are charged at once. No whole Char, SFF or pack is pinned.
	asset.bytes = int64(unsafe.Sizeof(*asset)) + int64(len(pal)*4) + 1024
	asset.pixels = int64(a.spr.Size[0]) * int64(a.spr.Size[1]) * int64(Max(1, int(a.spr.coldepth)/8))
	asset.owner = -1
	if root := sd.ovRoot; root != nil && a.sff != nil && root.gi() != nil && a.sff == root.gi().sff {
		asset.owner = root.id
		if ovReplay.owned[root.id] == nil {
			ovReplay.owned[root.id] = make(map[*ovReplayAsset]bool)
		}
		ovReplay.owned[root.id][asset] = true
	} else {
		asset.bytes += asset.pixels
	}
	ovReplay.assets[key] = asset
	ovReplay.unwarmed = append(ovReplay.unwarmed, asset)
	ovReplay.bytes += asset.bytes
	ovReplay.assetBytes += asset.bytes
	return asset
}

// ovReplayPalOf: Animation.pal's colours without its temporary SwapPalMap (a read-only copy of that path: the
// palette remap applies when SwapPalMap would swap - same length as the palette map)
func ovReplayPalOf(a *Animation, pfx *PalFX) []uint32 {
	pl := a.palettedata
	if pl == nil {
		pl = &a.sff.palList
	}
	s := a.spr
	if len(s.Pal) > 0 || s.coldepth > 8 {
		return s.Pal
	}
	m := pl.paletteMap
	if pfx != nil && len(pfx.remap) > 0 && len(pfx.remap) == len(pl.paletteMap) {
		m = pfx.remap
	}
	if len(m) == 0 {
		return nil
	}
	i := int(s.palidx)
	if i < 0 || i >= len(m) {
		i = 0
	}
	return pl.palettes[m[i]]
}

// ovReplayHint: 1 the asset hint is used (default), 0 off, 2 verify (both paths; ovReplayHintMisses counts answers
// that differ - logged with the replay stats)
var ovReplayHint = func() int {
	switch os.Getenv("IKEMEN_REPLAY_HINT") {
	case "0":
		return 0
	case "verify":
		return 2
	}
	return 1
}()
var ovReplayHintChecks, ovReplayHintMisses int64

// A root fighter leaves the match (ovDespawn, Ikemen thread, the host parked): the texels of its frozen sprites are
// the replay's alone from now on. The budget is enforced at the next capture.
func ovReplayOwnerGone(id int32) {
	if !ovReplay.enabled {
		return
	}
	for asset := range ovReplay.owned[id] {
		asset.owner = -1
		asset.bytes += asset.pixels
		ovReplay.bytes += asset.pixels
		ovReplay.assetBytes += asset.pixels
	}
	delete(ovReplay.owned, id)
	for sound := range ovReplay.ownedSamples[id] { // (its own sounds' WAVs too)
		if smp := ovReplay.samples[sound]; smp != nil && !smp.charged {
			smp.charged, smp.owner = true, -1
			ovReplay.bytes += int64(len(sound.wavData))
			ovReplay.sampleBytes += int64(len(sound.wavData))
		}
	}
	delete(ovReplay.ownedSamples, id)
}

// A slot's root while it is in the fight (ready), else nil: a root still loading is its loader goroutine's
func ovReplayLiveRoot(pn int) *Char {
	if pn < 0 || pn >= len(ov.slots) || (ovFFA && !ov.slots[pn].ready) {
		return nil
	}
	if pn >= len(sys.chars) || len(sys.chars[pn]) == 0 {
		return nil
	}
	return sys.chars[pn][0]
}

// The live root whose own SND holds a recorded sound (its Char id), or -1
func ovReplaySampleOwner(r *ovReplaySound) int32 {
	key := [2]int32{r.group, r.number}
	for pn := range sys.chars {
		c := ovReplayLiveRoot(pn)
		if c == nil || pn >= len(sys.cgi) || sys.cgi[pn].snd == nil {
			continue
		}
		if sys.cgi[pn].snd.table[key] == r.sound {
			return c.id
		}
	}
	return -1
}

func ovReplayKeepSample(r *ovReplaySound) {
	smp := ovReplay.samples[r.sound]
	if smp == nil {
		smp = &ovReplaySample{owner: ovReplaySampleOwner(r)}
		if smp.owner < 0 {
			smp.charged = true
			ovReplay.bytes += int64(len(r.sound.wavData))
			ovReplay.sampleBytes += int64(len(r.sound.wavData))
		} else {
			if ovReplay.ownedSamples[smp.owner] == nil {
				ovReplay.ownedSamples[smp.owner] = make(map[*Sound]bool)
			}
			ovReplay.ownedSamples[smp.owner][r.sound] = true
		}
		ovReplay.samples[r.sound] = smp
	}
	smp.refs++
}

func ovReplayReleaseSample(r *ovReplaySound) {
	smp := ovReplay.samples[r.sound]
	if smp == nil {
		return
	}
	if smp.refs--; smp.refs > 0 {
		return
	}
	delete(ovReplay.samples, r.sound)
	if smp.charged {
		ovReplay.bytes -= int64(len(r.sound.wavData))
		ovReplay.sampleBytes -= int64(len(r.sound.wavData))
	} else if owned := ovReplay.ownedSamples[smp.owner]; owned != nil {
		delete(owned, r.sound)
		if len(owned) == 0 {
			delete(ovReplay.ownedSamples, smp.owner)
		}
	}
}

// A sort-only entry: the SpriteData's order/sync fields, nothing drawn and nothing of the live world kept (never
// shared: there are few - sync members only)
func ovReplayBlank(sd *SpriteData) *ovReplaySprite {
	b := ovReplayNewEntry()
	c := *sd
	c.customShader = CustomShaderRenderData{} // (and every pointer zeroed by the pack)
	ovReplayPack(b, &c)
	ovReplay.bytes += ovReplayEntryBytes
	ovReplay.entryBytes += ovReplayEntryBytes
	return b
}

// ovReplayKeepDrawn: the replay's draw of e (as d) done - every value it wrote into d goes back into e's image (the
// order, sync and depth keys, interpolated positions: ovReplayRenderState restores what it restored before); e's
// pointers stay its own
func ovReplayKeepDrawn(e *ovReplaySprite, d *SpriteData) {
	shader := e.shader
	ovReplayPack(e, d)
	v := e.sd()
	v.sortindex, v.syncGroup = d.sortindex, d.syncGroup // (the pack clears them; ovReplayRenderState does after)
	if shader != nil && e.shader != nil && ovBytesEqual(shader, e.shader) {
		e.shader = shader // (unchanged: no new copy)
	}
}

func ovBytesEqual[T any](a, b *T) bool {
	n := int(unsafe.Sizeof(*a))
	return unsafe.String((*byte)(unsafe.Pointer(a)), n) == unsafe.String((*byte)(unsafe.Pointer(b)), n)
}

// The frozen look of a cued sprite: prev (the same animation's look in the newest frame) when nothing it draws
// changed, else a new one (its frozen sprite + resolved palette shared through ovReplayAssetFor as before). A new
// look has no references until an entry takes it.
func ovReplayLookFor(sd *SpriteData, prev *ovReplayLook) *ovReplayLook {
	var hint *ovReplayAsset
	if prev != nil && prev.refs > 0 {
		hint = prev.asset
	}
	asset := ovReplayAssetFor(sd, hint)
	var c ovReplayLook
	c.anim = *sd.anim
	c.frame[0] = sd.anim.frames[sd.anim.drawidx]
	c.frame[0].Clsn1, c.frame[0].Clsn2 = nil, nil
	a := &c.anim
	a.drawidx = 0
	a.sff, a.remap = nil, nil // the selected native sprite and resolved palette are already frozen
	a.interpolate_offset, a.interpolate_scale, a.interpolate_angle, a.interpolate_blend = nil, nil, nil, nil
	// (the animation clock: never drawn - zeroed so a held element compares equal from tick to tick)
	a.curtime, a.curelem, a.curelemtime, a.lastActionFrame = 0, 0, 0, 0
	a.totaltime, a.looptime, a.prelooptime, a.loopstart, a.copyAction = 0, 0, 0, 0, 0
	a.newframe, a.loopend, a.warnMissing = false, false, false
	a.spr, a.palettedata = &asset.sprite, &asset.palette
	if sd.pfx != nil {
		c.pfx = *sd.pfx
		c.pfx.remap = nil
		c.hasPfx = true
	}
	c.asset = asset
	if prev != nil && prev.refs > 0 && prev.asset == asset && prev.hasPfx == c.hasPfx {
		a.frames = prev.anim.frames
		if ovBytesEqual(&prev.anim, a) && ovBytesEqual(&prev.frame, &c.frame) && (!c.hasPfx || ovBytesEqual(&prev.pfx, &c.pfx)) {
			ovReplay.looksShared++
			return prev
		}
	}
	look := new(ovReplayLook)
	*look = c
	look.anim.frames = look.frame[:]
	asset.refs++
	ovReplay.bytes += ovReplayLookBytes
	ovReplay.lookBytes += ovReplayLookBytes
	ovReplay.looksNew++
	return look
}

func ovReplayReleaseLook(l *ovReplayLook) {
	if l == nil {
		return
	}
	if l.refs--; l.refs > 0 {
		return
	}
	ovReplay.bytes -= ovReplayLookBytes
	ovReplay.lookBytes -= ovReplayLookBytes
	a := l.asset
	l.asset = nil
	if a.refs--; a.refs == 0 {
		delete(ovReplay.assets, a.key)
		if a.owner >= 0 {
			delete(ovReplay.owned[a.owner], a)
			if len(ovReplay.owned[a.owner]) == 0 {
				delete(ovReplay.owned, a.owner)
			}
		}
		ovReplay.bytes -= a.bytes
		ovReplay.assetBytes -= a.bytes
	}
}

// The entry of a cued sprite in frame `tick`: the newest frame's entry of the same animation when its SpriteData and
// look are unchanged (and it is not placed in this frame already), else a new one. No references until the frame
// takes it.
func ovReplayEntryFor(sd *SpriteData, id uintptr, tick uint64) *ovReplaySprite {
	prev := ovReplay.prevEntries[id]
	if prev != nil && prev.refs <= 0 {
		prev = nil
	}
	var prevLook *ovReplayLook
	if prev != nil {
		prevLook = prev.look
	}
	look := ovReplayLookFor(sd, prevLook)
	var e ovReplaySprite
	ovReplayPack(&e, sd) // (its animation and PalFX are its look's: prev's are prev.look's)
	if prev != nil && prev.look == look && prev.usedAt != tick && ovReplaySameData(prev, &e) {
		prev.usedAt = tick
		ovReplay.entriesShared++
		return prev
	}
	n := ovReplayNewEntry()
	*n = e
	n.look, n.identity, n.usedAt = look, id, tick
	look.refs++
	ovReplay.bytes += ovReplayEntryBytes
	ovReplay.entryBytes += ovReplayEntryBytes
	ovReplay.entriesNew++
	return n
}

func ovReplayReleaseEntry(e *ovReplaySprite) {
	if e.refs--; e.refs > 0 {
		return
	}
	ovReplay.bytes -= ovReplayEntryBytes
	ovReplay.entryBytes -= ovReplayEntryBytes
	ovReplayReleaseLook(e.look)
	e.look = nil
	ovReplayFreeEntry(e)
}

// The frame's draw list: its entries' SpriteData (each pointing at its look's frozen animation and PalFX). Since the GC
// pass it is made when the frame is shown (ovReplayDrawList); here only the capacity the kept list had (it grew to the
// frame's length, else stayed) for ovReplayFrameBytes
func ovReplayBindList(f *ovReplayFrame) {
	if f.listCap < len(f.sprites) {
		f.listCap = len(f.sprites)
	}
}

// ovReplayDrawList: (the replay's draw) f's entries as whole SpriteData in a reused scratch, in the frame's order
var ovReplayScratch []SpriteData
var ovReplayScratchList DrawList

func ovReplayDrawList(f *ovReplayFrame) DrawList {
	n := len(f.sprites)
	if cap(ovReplayScratch) < n {
		ovReplayScratch = make([]SpriteData, n+n/8)
		ovReplayScratchList = make(DrawList, n+n/8)
	}
	sc, list := ovReplayScratch[:n], ovReplayScratchList[:n]
	for i, e := range f.sprites {
		ovReplayUnpack(e, &sc[i])
		list[i] = &sc[i]
	}
	return list
}

func ovReplayFrameBytes(f *ovReplayFrame) int64 {
	return int64(unsafe.Sizeof(ovReplayFrameSized{})) + int64(cap(f.sprites))*8 + int64(f.listCap)*8 +
		int64(len(f.sounds))*int64(unsafe.Sizeof(ovReplaySound{})) + int64(len(f.poses))*64
}

// (the frame's layout before the GC pass - a DrawList where listCap is now - for the budget's accounting only)
type ovReplayFrameSized struct {
	tick            uint64
	refs            int
	bytes           int64
	pruned          bool
	sprites         []*ovReplaySprite
	list            DrawList
	cam             Camera
	shake           EnvShake
	allFX           PalFX
	superTime       int32
	superBrightness float32
	gameTick        int
	frameCounter    int32
	sounds          []ovReplaySound
	poses           map[int]ovReplayPose
}

// The pruning view (init config "replay_prune", off by default): it assumes the host shows a clip with a camera that
// stands in front of the clip's fighters (+ ik z) and looks back at them. A sprite is kept when its anchor lies in a
// wedge around a clip's victim or killer: its apex ovViewApex in front of the fighter, its half-width per unit of
// distance ovViewSlope, ovViewMargin on each side for a sprite's own reach - what would stand beside or behind such
// a camera goes. The wedge has no far limit, so fighters in the background stay.
const (
	ovViewApex   = 480.0 // ik units in front of the subject
	ovViewSlope  = 1.19  // 1.5 x the tangent of half a 76.7 degree horizontal field of view
	ovViewMargin = 200.0 // ik units
)

func ovReplayInView(x, z float32, subjects [][2]float32) bool {
	for _, at := range subjects {
		dx, dz := x-at[0], z-at[1]
		if d := ovViewApex - dz; d > 0 && Abs(dx) <= ovViewSlope*d+ovViewMargin {
			return true
		}
	}
	return false
}

// Capture in two phases: the costly part of every cued sprite's entry - resolving its palette and
// asset, building its normalized look and comparing it and its SpriteData with the same animation's look and entry
// of the newest frame - only reads (the parked world, the newest frame's entries, looks and assets and the asset map,
// none of which change before the serial phase), so it runs on several goroutines (ovReplayPrepare); the serial
// phase (ovReplayEntryPre) then takes the same decisions as ovReplayEntryFor in list order from those results,
// deciding what only it can know: an asset made by an earlier sprite of this frame, an entry already placed in this
// frame, the reference counts. IKEMEN_REPLAY_PAR=0: the one-phase ovReplayEntryFor; =verify: both, each prediction
// checked against ovReplayEntryFor's actual result (ovReplayParChecks, ovReplayParMisses).
type ovReplayPre struct {
	ok       bool
	prev     *ovReplaySprite // the newest frame's entry of the same animation, still held (refs > 0)
	asset    *ovReplayAsset  // the existing asset of its colours (nil: none yet - found or made in the serial phase)
	lookSame bool            // prev's look draws it unchanged (asset is that look's asset)
	dataSame bool            // with prev's look, prev's SpriteData is unchanged too
	look     ovReplayLook    // the new look when !lookSame (its sprite and palette set once its asset is known)
}

var ovReplayPar = func() int {
	switch os.Getenv("IKEMEN_REPLAY_PAR") {
	case "0":
		return 0
	case "verify":
		return 2
	}
	return 1
}()
var ovReplayPres []ovReplayPre
var ovReplayParChecks, ovReplayParMisses int64

const ovReplayParWorkers = 4

func ovReplayPrepare(list DrawList) []ovReplayPre {
	if ovReplayPar == 0 || len(list) < 64 {
		return nil
	}
	if cap(ovReplayPres) < len(list) {
		ovReplayPres = make([]ovReplayPre, len(list)+len(list)/4)
	}
	pres := ovReplayPres[:len(list)]
	var wg sync.WaitGroup
	var failed atomic.Bool
	chunk := (len(list) + ovReplayParWorkers - 1) / ovReplayParWorkers
	for lo := 0; lo < len(list); lo += chunk {
		hi := Min(lo+chunk, len(list))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			defer func() { // (a panic here would end the host: the one-phase path meets it on the host thread instead,
				// where ikemen_step_end's recovery reports it as before)
				if recover() != nil {
					failed.Store(true)
				}
			}()
			for i := lo; i < hi; i++ {
				ovReplayPrepareOne(list[i], &pres[i])
			}
		}(lo, hi)
	}
	wg.Wait()
	if failed.Load() {
		return nil
	}
	return pres
}

// ovReplayPrepareOne: read-only (see ovReplayPre); the read part of ovReplayEntryFor/ovReplayLookFor/ovReplayAssetFor
func ovReplayPrepareOne(sd *SpriteData, pre *ovReplayPre) {
	pre.ok, pre.prev, pre.asset, pre.lookSame, pre.dataSame = false, nil, nil, false, false
	if sd == nil || sd.isBlank() || sd.screen || sd.anim.drawidx < 0 || int(sd.anim.drawidx) >= len(sd.anim.frames) {
		return // (the huge cut writes its explod's scale record: decided in the serial phase, as before)
	}
	pre.ok = true
	prev := ovReplay.prevEntries[uintptr(unsafe.Pointer(sd.anim))]
	if prev != nil && prev.refs <= 0 {
		prev = nil
	}
	pre.prev = prev
	var pl *ovReplayLook
	var hint *ovReplayAsset
	if prev != nil {
		pl = prev.look
	}
	if pl != nil && pl.refs > 0 {
		hint = pl.asset
	}
	asset := ovReplayAssetFind(sd, hint)
	pre.asset = asset
	c := &pre.look
	*c = ovReplayLook{}
	c.anim = *sd.anim
	c.frame[0] = sd.anim.frames[sd.anim.drawidx]
	c.frame[0].Clsn1, c.frame[0].Clsn2 = nil, nil
	a := &c.anim
	a.drawidx = 0
	a.sff, a.remap = nil, nil
	a.interpolate_offset, a.interpolate_scale, a.interpolate_angle, a.interpolate_blend = nil, nil, nil, nil
	a.curtime, a.curelem, a.curelemtime, a.lastActionFrame = 0, 0, 0, 0
	a.totaltime, a.looptime, a.prelooptime, a.loopstart, a.copyAction = 0, 0, 0, 0, 0
	a.newframe, a.loopend, a.warnMissing = false, false, false
	a.spr, a.palettedata = nil, nil
	if asset != nil {
		a.spr, a.palettedata = &asset.sprite, &asset.palette
	}
	if sd.pfx != nil {
		c.pfx = *sd.pfx
		c.pfx.remap = nil
		c.hasPfx = true
	}
	c.asset = asset
	if asset == nil || pl == nil || pl.refs <= 0 || pl.asset != asset || pl.hasPfx != c.hasPfx {
		return
	}
	a.frames = pl.anim.frames
	if !(ovBytesEqual(&pl.anim, a) && ovBytesEqual(&pl.frame, &c.frame) && (!c.hasPfx || ovBytesEqual(&pl.pfx, &c.pfx))) {
		return
	}
	pre.lookSame = true
	var e ovReplaySprite
	ovReplayPack(&e, sd) // (with prev's look: its animation and PalFX are prev's too)
	pre.dataSame = ovReplaySameData(prev, &e)
}

// ovReplayAssetFind: ovReplayAssetFor's answer without making an asset (nil where it would make one)
func ovReplayAssetFind(sd *SpriteData, hint *ovReplayAsset) *ovReplayAsset {
	a := sd.anim
	pal := ovReplayPalOf(a, sd.pfx)
	if hint != nil && ovReplayHint != 0 && hint.refs > 0 && hint.key.source == a.spr && len(pal) > 0 &&
		len(hint.srcPal) == len(pal) && &hint.srcPal[0] == &pal[0] {
		return hint
	}
	var h uint64
	if len(pal) > 0 {
		h = maphash.Bytes(ovReplaySeed, unsafe.Slice((*byte)(unsafe.Pointer(&pal[0])), len(pal)*4))
	}
	key := ovReplayAssetKey{source: a.spr, pal: h}
	for ; ; key.n++ {
		asset := ovReplay.assets[key]
		if asset == nil {
			return nil
		}
		if ovU32Equal(asset.sprite.Pal, pal) {
			return asset
		}
	}
}

// ovReplayEntryPre: ovReplayEntryFor's decisions from the prepared results
func ovReplayEntryPre(sd *SpriteData, id uintptr, tick uint64, pre *ovReplayPre) *ovReplaySprite {
	prev := pre.prev
	var look *ovReplayLook
	if pre.lookSame { // (prev's look is still held: references only grow during the capture)
		look = prev.look
		ovReplay.looksShared++
	} else {
		asset := pre.asset
		if asset == nil {
			// None existed when prepared: one made by an earlier sprite of this frame, or a new one (the same chain
			// search and creation as the one-phase path). It cannot be prev's look's asset, which existed already.
			asset = ovReplayAssetFor(sd, nil)
			pre.look.anim.spr, pre.look.anim.palettedata = &asset.sprite, &asset.palette
			pre.look.asset = asset
		}
		look = new(ovReplayLook)
		*look = pre.look
		look.anim.frames = look.frame[:]
		asset.refs++
		ovReplay.bytes += ovReplayLookBytes
		ovReplay.lookBytes += ovReplayLookBytes
		ovReplay.looksNew++
	}
	if prev != nil && prev.look == look && prev.usedAt != tick && pre.dataSame {
		prev.usedAt = tick
		ovReplay.entriesShared++
		return prev
	}
	var e ovReplaySprite
	ovReplayPack(&e, sd)
	n := ovReplayNewEntry()
	*n = e
	n.look, n.identity, n.usedAt = look, id, tick
	look.refs++
	ovReplay.bytes += ovReplayEntryBytes
	ovReplay.entryBytes += ovReplayEntryBytes
	ovReplay.entriesNew++
	return n
}

// ovReplayEntryVerify: (IKEMEN_REPLAY_PAR=verify) the one-phase result, the predictions checked against it: the look
// kept or new, the asset, the entry kept or new, and a new look's content (anim, frame, PalFX) as prepared
func ovReplayEntryVerify(sd *SpriteData, id uintptr, tick uint64, pre *ovReplayPre) *ovReplaySprite {
	prev := pre.prev
	free := prev != nil && prev.usedAt != tick
	e := ovReplayEntryFor(sd, id, tick)
	miss := pre.lookSame != (prev != nil && e.look == prev.look)
	if pre.asset != nil && e.look.asset != pre.asset {
		miss = true
	}
	if (pre.lookSame && pre.dataSame && free) != (e == prev) {
		miss = true
	}
	if !pre.lookSame && e.look != nil && e.look.asset == pre.asset {
		c := pre.look
		c.anim.frames = e.look.anim.frames
		if !ovBytesEqual(&c.anim, &e.look.anim) || !ovBytesEqual(&c.frame, &e.look.frame) || c.hasPfx != e.look.hasPfx ||
			c.hasPfx && !ovBytesEqual(&c.pfx, &e.look.pfx) {
			miss = true
		}
	}
	atomic.AddInt64(&ovReplayParChecks, 1)
	if miss {
		atomic.AddInt64(&ovReplayParMisses, 1)
	}
	return e
}

// A history frame leaves the pre-roll ring (no new clip can take it any more) while clips still hold it: its entries
// are cut to what those clips' camera can show (the pruning view, above). Blank sync metadata stays where it orders
// surviving sprites.
func ovReplayPrune(f *ovReplayFrame) {
	if f.pruned || !ovReplay.pruneOn {
		return
	}
	f.pruned = true
	var buf [64][2]float32
	subjects := buf[:0]
	keepPose := map[int]bool{}
	for _, c := range ovReplay.clips {
		if len(c.frames) == 0 || c.frames[0].tick > f.tick || c.frames[len(c.frames)-1].tick < f.tick {
			continue
		}
		for k, h := range [2]int{c.victim, c.killer} {
			keepPose[h] = true
			id := c.victimID
			if k == 1 {
				id = c.killerID
			}
			if p, ok := f.poses[h]; h >= 0 && ok && (id < 0 || p.id == id) {
				c.viewAt[k] = [2]float32{p.at[0] - ovFloorX0, (p.at[1] - ovFloorD0) * ovDepthScale}
				c.viewOK[k] = true
			}
			if c.viewOK[k] && len(subjects) < cap(subjects) {
				subjects = append(subjects, c.viewAt[k])
			}
		}
	}
	if len(subjects) == 0 {
		return // (no subject position known: kept whole)
	}
	ovReplay.bytes -= f.bytes
	kept := f.sprites[:0]
	total, inView := 0, 0
	for _, e := range f.sprites {
		if e.look != nil {
			total++
			if v := e.sd(); ovReplayInView(v.ovX, v.ovZ, subjects) {
				inView++
				kept = append(kept, e)
				continue
			}
		}
		if e.sd().syncId > 0 { // (sort metadata: a blank of its own)
			b := ovReplayBlank(e.sd())
			b.refs++
			kept = append(kept, b)
		}
		ovReplayReleaseEntry(e)
	}
	// (exact-size storage: the history frame's big slice goes back to the pool, cleared)
	old := f.sprites
	f.sprites = append(make([]*ovReplaySprite, 0, len(kept)), kept...)
	for i := range old[:cap(old)] {
		old[:cap(old)][i] = nil
	}
	if len(ovReplaySpritePool) < 6 {
		ovReplaySpritePool = append(ovReplaySpritePool, old[:0])
	}
	f.listCap = 0
	ovReplayBindList(f)
	poses := make(map[int]ovReplayPose, len(keepPose)) // (the clips holding it follow only their victims and killers now)
	for pn := range keepPose {
		if p, ok := f.poses[pn]; ok {
			poses[pn] = p
		}
	}
	f.poses = poses
	f.bytes = ovReplayFrameBytes(f)
	ovReplay.bytes += f.bytes
	ovReplay.pruned++
	ovReplay.pruneTotal += int64(total)
	ovReplay.pruneKept += int64(inView)
}

// Called at the end of ovStep, after cueDraw and before any host command can
// despawn/mutate an owner. No GL calls, no image decoding, no shader uploads.
func ovReplayCapture() {
	if !ovReplay.enabled || ovReplay.playing != nil {
		return
	}
	start := time.Now()
	f := &ovReplayFrame{tick: ovTicks, cam: sys.cam, shake: sys.envShake,
		allFX: *sys.allPalFX, superTime: sys.supertime, superBrightness: sys.superbrightness,
		gameTick: sys.tickCount, frameCounter: sys.frameCounter}
	f.allFX.remap = nil
	f.cam.FollowChar = nil
	f.poses = make(map[int]ovReplayPose)
	wpp := ovWPP()
	for pn := range sys.chars {
		// (only roots in the fight - as ovFillState: a root still loading belongs to its loader goroutine, which writes
		// its localscl/SND while this runs)
		c := ovReplayLiveRoot(pn)
		if c == nil || wpp <= 0 {
			continue
		}
		k := c.localscl / wpp
		// (as ovFillState's record)
		f.poses[pn] = ovReplayPose{at: [4]float32{c.pos[0]*k + ovFloorX0, c.pos[2]*k/ovDepthScale + ovFloorD0,
			-c.pos[1] * k, 0}, id: c.id}
	}
	f.sprites = ovReplayTakeSprites(len(sys.spriteList))
	next := ovReplay.nextEntries
	pres := ovReplayPrepare(sys.spriteList)
	for i, sd := range sys.spriteList {
		if sd == nil {
			continue
		}
		var e *ovReplaySprite
		if sd.isBlank() || sd.screen || (ovHugeOn && ovHugeCut(sd)) ||
			sd.anim.drawidx < 0 || int(sd.anim.drawidx) >= len(sd.anim.frames) {
			// Not drawn. A sync member keeps its sort metadata: DrawList assigns sync groups before its blank/screen/
			// huge filter, and a group's first member decides the group's place. Any other entry leaves the list
			// without reordering the rest (every other key is its own index, shifted alike).
			if sd.syncId <= 0 {
				continue
			}
			e = ovReplayBlank(sd)
		} else {
			id := uintptr(unsafe.Pointer(sd.anim))
			switch {
			case pres == nil || !pres[i].ok:
				e = ovReplayEntryFor(sd, id, f.tick)
			case ovReplayPar == 2:
				e = ovReplayEntryVerify(sd, id, f.tick, &pres[i])
			default:
				e = ovReplayEntryPre(sd, id, f.tick, &pres[i])
			}
			next[id] = e
			if sd.ovRoot != nil {
				e.owner = sd.ovRoot.id + 1
			}
			if sd.ovKind == 0 && sd.ovRoot != nil {
				if p, ok := f.poses[sd.ovRoot.playerNo]; ok && p.id == sd.ovRoot.id {
					p.at[3] = 1
					f.poses[sd.ovRoot.playerNo] = p
				}
			}
		}
		e.refs++
		f.sprites = append(f.sprites, e)
	}
	for k := range ovReplay.prevEntries { // (go1.20: no clear builtin)
		delete(ovReplay.prevEntries, k)
	}
	ovReplay.prevEntries, ovReplay.nextEntries = next, ovReplay.prevEntries
	ovReplaySlotsBoundary()
	ovReplayBindList(f)
	f.sounds = ovReplaySoundSnapshot()
	f.bytes = ovReplayFrameBytes(f)
	for i := range f.sounds {
		if f.sounds[i].sound != nil {
			ovReplayKeepSample(&f.sounds[i])
		}
	}
	ovReplay.bytes += f.bytes
	ovReplayKeep(f)
	ovReplay.ring = append(ovReplay.ring, f)
	for _, clip := range ovReplay.clips {
		if clip.open {
			ovReplayKeep(f)
			clip.frames = append(clip.frames, f)
			if len(clip.frames) > ovReplayMaxTicks {
				ovReplayDrop(clip.id, "tick_limit")
			}
		}
	}
	for len(ovReplay.ring) > ovReplayPreRoll+1 {
		ovReplayPopRing()
	}
	ovReplayEnforceBudget()
	us := time.Since(start).Microseconds()
	ovReplay.captureUS += us
	ovReplay.captures++
	if ovReplayPar == 2 && ovReplay.captures%600 == 0 { // (IKEMEN_REPLAY_PAR=verify: every 10 s)
		LogMessage("[overlay] replay two-phase capture verify: %d checks, %d misses", atomic.LoadInt64(&ovReplayParChecks),
			atomic.LoadInt64(&ovReplayParMisses))
	}
	if ovReplayHint == 2 && ovReplay.captures%600 == 0 { // (IKEMEN_REPLAY_HINT=verify: every 10 s)
		LogMessage("[overlay] replay asset hint verify: %d checks, %d misses", atomic.LoadInt64(&ovReplayHintChecks),
			atomic.LoadInt64(&ovReplayHintMisses))
	}
	if us > ovReplay.captureMaxUS {
		ovReplay.captureMaxUS = us
	}
	if ovReplay.bytes > ovReplay.peak {
		ovReplay.peak = ovReplay.bytes
	}
}

func ovReplayKeep(f *ovReplayFrame) { f.refs++; ovReplay.bytes += 8 }

func ovReplayRelease(f *ovReplayFrame) {
	ovReplay.bytes -= 8
	f.refs--
	if f.refs != 0 {
		return
	}
	ovReplay.bytes -= f.bytes
	defer ovReplayPoolSprites(f)
	for _, e := range f.sprites {
		ovReplayReleaseEntry(e)
	}
	for i := range f.sounds {
		if f.sounds[i].sound != nil {
			ovReplayReleaseSample(&f.sounds[i])
		}
	}
}

// Released frames' draw-entry storage is reused (~0.45 MB a tick at 32 fighters, ~1 MB at a hundred, was
// new garbage every tick). Cleared on return: no stale pointer keeps a released sprite or texture alive.
var ovReplaySpritePool [][]*ovReplaySprite

func ovReplayTakeSprites(n int) []*ovReplaySprite {
	for i := len(ovReplaySpritePool) - 1; i >= 0; i-- {
		if s := ovReplaySpritePool[i]; cap(s) >= n {
			ovReplaySpritePool[i] = ovReplaySpritePool[len(ovReplaySpritePool)-1]
			ovReplaySpritePool[len(ovReplaySpritePool)-1] = nil
			ovReplaySpritePool = ovReplaySpritePool[:len(ovReplaySpritePool)-1]
			return s[:0]
		}
	}
	return make([]*ovReplaySprite, 0, n+n/8)
}

func ovReplayPoolSprites(f *ovReplayFrame) {
	s := f.sprites
	f.sprites, f.listCap = nil, 0
	if f.pruned || cap(s) == 0 || len(ovReplaySpritePool) >= 6 { // (a pruned frame's slice is mostly empty capacity)
		return
	}
	s = s[:cap(s)]
	for i := range s {
		s[i] = nil
	}
	ovReplaySpritePool = append(ovReplaySpritePool, s[:0])
}

func ovReplayPopRing() {
	f := ovReplay.ring[0]
	ovReplay.ring[0] = nil
	ovReplay.ring = ovReplay.ring[1:]
	if f.refs > 1 { // (still held by clips)
		ovReplayPrune(f)
	}
	ovReplayRelease(f)
}

func ovReplayDrop(id int, reason string) {
	c := ovReplay.clips[id]
	if c == nil || c == ovReplay.playing {
		return
	}
	delete(ovReplay.clips, id)
	var t0, entries int64
	if ovTickLog != nil {
		for _, f := range c.frames {
			entries += int64(len(f.sprites))
		}
		t0 = ovTimingUs()
	}
	ovReplayReleaseFrames(c.frames)
	if ovTickLog != nil { // (dev, IKEMEN_TICK_LOG: a dropped clip's release - start, end, frames, entries, reason length)
		ovTickLog.line('D', t0, ovTimingUs(), int64(len(c.frames)), entries, int64(len(reason)), ovReplayDropPhases[0],
			ovReplayDropPhases[1], ovReplayDropPhases[2], ovReplayDropReleased)
		ovReplayDropPhases, ovReplayDropReleased = [3]int64{}, 0
	}
	if reason != "host" {
		ovReplay.dropped++
		ovPushEvent(map[string]interface{}{"e": "clip_dropped", "id": id, "reason": reason})
	}
}

// A dropped clip lets go of up to a few hundred frames of thousands of entries each - one reference drop per entry,
// each a cache miss, on the join (the host's frame). The drops of the entries' references are spread over goroutines
// (atomic); everything else ovReplayRelease does stays serial and in its order. The same end
// state: an entry is released exactly when a drop of its count leaves it <= 0 (as ovReplayReleaseEntry), each such drop
// once; what a release changes - integer byte counters (sums), look/asset counts, map deletions - does not depend on the
// order the released entries are handled in; frame references, sounds and the storage pool go frame by frame in order.
// Env IKEMEN_REPLAY_PAR_DROP=0: serial.
var ovReplayParDrop = os.Getenv("IKEMEN_REPLAY_PAR_DROP") != "0"
var ovReplayDropPhases [3]int64 // (dev, IKEMEN_TICK_LOG: the parallel release's phase ends...)
var ovReplayDropReleased int64  // (...and the entries it released)

func ovReplayReleaseFrames(frames []*ovReplayFrame) {
	if !ovReplayParDrop || len(frames) < 16 {
		for _, f := range frames {
			ovReplayRelease(f)
		}
		return
	}
	// ovReplayRelease's head for every frame, in order: the frames this drop frees
	freed := make([]*ovReplayFrame, 0, len(frames))
	entries := 0
	for _, f := range frames {
		ovReplay.bytes -= 8
		f.refs--
		if f.refs != 0 {
			continue
		}
		ovReplay.bytes -= f.bytes
		freed = append(freed, f)
		entries += len(f.sprites)
	}
	if ovTickLog != nil {
		ovReplayDropPhases[0] = ovTimingUs()
	}
	// the entries' reference drops (ovReplayReleaseEntry's first line), on workers by contiguous runs of frames
	workers := 1
	if entries >= 8192 { // (every P: the Ikemen thread is parked at the join, the state workers idle)
		workers = Clamp(runtime.GOMAXPROCS(0), 2, 8)
	}
	released := make([][]*ovReplaySprite, workers)
	run := func(w int) {
		var z []*ovReplaySprite
		for i := w; i < len(freed); i += workers {
			for _, e := range freed[i].sprites {
				if atomic.AddInt64((*int64)(unsafe.Pointer(&e.refs)), -1) <= 0 {
					z = append(z, e)
				}
			}
		}
		released[w] = z
	}
	ovParDo(workers, run)
	if ovTickLog != nil {
		ovReplayDropPhases[1] = ovTimingUs()
	}
	// the rest of ovReplayReleaseEntry for each released entry. An entry whose drop left exactly 0 is released by one
	// worker alone (a count passes 0 once): its byte counters summed per worker, its look's reference dropped atomically
	// and the looks that reach 0 collected; a drop below 0 (never seen: a count dropped more often than held) follows on
	// one thread as ovReplayReleaseEntry would - its look already nil, only the byte counters
	type lookSums struct {
		looks            []*ovReplayLook   // looks whose count went below 0 (never seen): ovReplayReleaseLook's way, serially
		free             []*ovReplaySprite // released slots, waiting for reuse
		assets           []*ovReplayAsset  // assets whose count reached exactly 0 (once each)
		neg              []*ovReplaySprite
		bytes, lookFreed int64
	}
	sums := make([]lookSums, workers)
	tail := func(w int) {
		var ls lookSums
		for _, e := range released[w] {
			if e.refs < 0 {
				ls.neg = append(ls.neg, e)
				continue
			}
			ls.bytes += ovReplayEntryBytes
			if e.refs == 0 { // (its slot waits for reuse - ovReplaySlotsBoundary)
				e.shader = nil
				ls.free = append(ls.free, e)
			}
			l := e.look
			e.look = nil
			if l == nil {
				continue
			}
			switch n := atomic.AddInt64((*int64)(unsafe.Pointer(&l.refs)), -1); {
			case n == 0: // (ovReplayReleaseLook past its count, by this worker alone: the count passed 0 here)
				ls.lookFreed += ovReplayLookBytes
				a := l.asset
				l.asset = nil
				if atomic.AddInt64((*int64)(unsafe.Pointer(&a.refs)), -1) == 0 {
					ls.assets = append(ls.assets, a)
				}
			case n < 0:
				ls.looks = append(ls.looks, l)
			}
		}
		sums[w] = ls
	}
	ovParDo(workers, tail)
	for w := range sums {
		ovReplayDropReleased += int64(len(released[w]))
		ovReplay.bytes -= sums[w].bytes + sums[w].lookFreed
		ovReplay.entryBytes -= sums[w].bytes
		ovReplay.lookBytes -= sums[w].lookFreed
	}
	for w := range sums {
		for _, e := range sums[w].neg {
			ovReplay.bytes -= ovReplayEntryBytes
			ovReplay.entryBytes -= ovReplayEntryBytes
			ovReplayReleaseLook(e.look)
			e.look = nil
		}
	}
	for w := range sums { // (each worker's released slots wait for reuse as its list - no copy)
		if len(sums[w].free) > 0 {
			ovReplayFreeing = append(ovReplayFreeing, sums[w].free)
		}
	}
	for w := range sums { // (the rest of ovReplayReleaseLook for each asset whose count reached 0)
		for _, a := range sums[w].assets {
			delete(ovReplay.assets, a.key)
			if a.owner >= 0 {
				delete(ovReplay.owned[a.owner], a)
				if len(ovReplay.owned[a.owner]) == 0 {
					delete(ovReplay.owned, a.owner)
				}
			}
			ovReplay.bytes -= a.bytes
			ovReplay.assetBytes -= a.bytes
		}
	}
	for w := range sums { // (looks dropped below 0 - never seen - as ovReplayReleaseLook past its count)
		for _, l := range sums[w].looks {
			ovReplay.bytes -= ovReplayLookBytes
			ovReplay.lookBytes -= ovReplayLookBytes
			a := l.asset
			l.asset = nil
			if a.refs--; a.refs == 0 {
				delete(ovReplay.assets, a.key)
				if a.owner >= 0 {
					delete(ovReplay.owned[a.owner], a)
					if len(ovReplay.owned[a.owner]) == 0 {
						delete(ovReplay.owned, a.owner)
					}
				}
				ovReplay.bytes -= a.bytes
				ovReplay.assetBytes -= a.bytes
			}
		}
	}
	if ovTickLog != nil {
		ovReplayDropPhases[2] = ovTimingUs()
	}
	// ovReplayRelease's tail: the frames' sounds. The samples map is only read here (no writer meanwhile); each sample's
	// count dropped atomically; the samples that reach <= 0 are let go in the original way afterwards, once each (as
	// ovReplayReleaseSample, whose later calls for an already deleted sample find nothing)
	type sampleDrop struct {
		sound *Sound
		smp   *ovReplaySample
	}
	gone := make([][]sampleDrop, workers)
	sounds := func(w int) {
		var z []sampleDrop
		for i := w; i < len(freed); i += workers {
			f := freed[i]
			for j := range f.sounds {
				r := &f.sounds[j]
				if r.sound == nil {
					continue
				}
				smp := ovReplay.samples[r.sound]
				if smp != nil && atomic.AddInt64((*int64)(unsafe.Pointer(&smp.refs)), -1) <= 0 {
					z = append(z, sampleDrop{r.sound, smp})
				}
			}
		}
		gone[w] = z
	}
	ovParDo(workers, sounds)
	for w := range gone {
		for _, g := range gone[w] {
			if ovReplay.samples[g.sound] != g.smp {
				continue // (already let go: the original's next call would have found nothing)
			}
			delete(ovReplay.samples, g.sound)
			if g.smp.charged {
				ovReplay.bytes -= int64(len(g.sound.wavData))
				ovReplay.sampleBytes -= int64(len(g.sound.wavData))
			} else if owned := ovReplay.ownedSamples[g.smp.owner]; owned != nil {
				delete(owned, g.sound)
				if len(owned) == 0 {
					delete(ovReplay.ownedSamples, g.smp.owner)
				}
			}
		}
	}
	// ...and the storage pool, frame by frame in order (its contents and order feed later frames' byte accounting)
	for _, f := range freed {
		ovReplayPoolSprites(f)
	}
}

// ovParDo: f(0..workers-1), on goroutines when there is more than one
func ovParDo(workers int, f func(w int)) {
	if workers == 1 {
		f(0)
		return
	}
	// (a worker's panic - reachable only through a reference count already wrong - is caught on its goroutine and raised
	// again here, on the calling thread, with the worker's stack: the caller's ovRecover handles it as it handled the
	// serial release's; uncaught on a worker it would end the host's process)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failure interface{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					if failure == nil {
						failure = fmt.Sprintf("%v (on replay worker %d)\n%s", r, w, debug.Stack())
					}
					mu.Unlock()
				}
			}()
			f(w)
		}(w)
	}
	wg.Wait()
	if failure != nil {
		panic(failure)
	}
}

func ovReplayEnforceBudget() {
	for ovReplay.bytes > ovReplay.budget && len(ovReplay.ring) > 0 {
		ovReplayPopRing()
	}
	for ovReplay.bytes > ovReplay.budget {
		var weakest *ovReplayClip
		for _, c := range ovReplay.clips {
			if c == ovReplay.playing {
				continue
			}
			if weakest == nil || c.score < weakest.score || (c.score == weakest.score && c.id < weakest.id) {
				weakest = c
			}
		}
		if weakest == nil {
			break
		}
		ovReplayDrop(weakest.id, "memory_budget")
	}
}

func ovReplayStats() {
	// (the newest history frame's draw entries, and how many of them are blank sort-only entries)
	sprites, blanks := 0, 0
	if n := len(ovReplay.ring); n > 0 {
		f := ovReplay.ring[n-1]
		sprites = len(f.sprites)
		for _, e := range f.sprites {
			if e.look == nil {
				blanks++
			}
		}
	}
	ovPushEvent(map[string]interface{}{"e": "replay_stats", "bytes": ovReplay.bytes, "peak_bytes": ovReplay.peak,
		"budget_bytes": ovReplay.budget, "clips": len(ovReplay.clips), "history_ticks": len(ovReplay.ring),
		"assets": len(ovReplay.assets), "samples": len(ovReplay.samples), "captures": ovReplay.captures,
		"capture_us": ovReplay.captureUS, "capture_max_us": ovReplay.captureMaxUS,
		"hint_checks": atomic.LoadInt64(&ovReplayHintChecks), "hint_misses": atomic.LoadInt64(&ovReplayHintMisses), // (IKEMEN_REPLAY_HINT=verify)
		"audio_plays": ovReplay.audioPlays, "audio_stops": ovReplay.audioStops,
		"repeat_shows": ovReplay.repeatedShows, "dropped": ovReplay.dropped, "playing": ovReplayIsPlaying(),
		"asset_bytes": ovReplay.assetBytes, "sample_bytes": ovReplay.sampleBytes,
		"sprite_size": int(unsafe.Sizeof(ovReplaySprite{})), "frame_sprites": sprites, "frame_blanks": blanks,
		"look_bytes": ovReplay.lookBytes, "look_size": ovReplayLookBytes, "looks_new": ovReplay.looksNew,
		"looks_shared": ovReplay.looksShared, "pruned_frames": ovReplay.pruned, "prune_kept": ovReplay.pruneKept,
		"prune_total": ovReplay.pruneTotal, "entry_bytes": ovReplay.entryBytes, "entries_new": ovReplay.entriesNew,
		"entries_shared": ovReplay.entriesShared, "owned_samples": len(ovReplay.ownedSamples)})
}

// The replay's own ops (ikemen_command routes by the op it has already parsed).
func ovReplayOp(op string) bool {
	switch op {
	case "clip_open", "clip_close", "clip_drop", "clip_score", "clip_ready", "replay_begin", "replay_show", "replay_end", "replay_stats", "replay_clear", "replay_tick":
		return true
	}
	return false
}

// Returns handled separately so ordinary core commands retain their existing
// ABI and timing. replay_show's tick is clip-relative, and may be fractional.
func ovReplayCommand(js []byte) (bool, int, error) {
	var p struct {
		Op     string   `json:"op"`
		ID     int      `json:"id"`
		Tick   float64  `json:"tick"`
		From   *uint64  `json:"from"`
		Score  *float64 `json:"score"`
		Pre    *int     `json:"pre_roll"`
		Victim int      `json:"h"`
		Killer *int     `json:"k"` // the handle of the fighter that struck the clip's fighter (the clip's activity, pruning)
	}
	if err := json.Unmarshal(js, &p); err != nil {
		return false, 0, err
	}
	if !ovReplayOp(p.Op) {
		return false, 0, nil
	}
	if !ovReplay.enabled {
		return true, -1, fmt.Errorf("replay recording is disabled (config replay=true)")
	}
	c := ovReplay.clips[p.ID]
	switch p.Op {
	case "replay_tick":
		return true, int(ovTicks), nil
	case "clip_open":
		if ovReplayIsPlaying() {
			return true, -1, fmt.Errorf("clip_open during replay")
		}
		if c != nil {
			return true, -1, fmt.Errorf("clip %d already exists", p.ID)
		}
		if len(ovReplay.clips) >= 32 {
			return true, -1, fmt.Errorf("clip limit (32)")
		}
		pre := ovReplayPreRoll
		if p.Pre != nil {
			pre = Max(0, Min(*p.Pre, ovReplayPreRoll))
		}
		c = &ovReplayClip{id: p.ID, open: true, victim: p.Victim, victimID: -1, killer: -1, killerID: -1}
		if v := ovReplayLiveRoot(p.Victim); v != nil {
			c.victimID = v.id
		}
		ovReplaySetKiller(c, p.Killer)
		if p.Score != nil {
			c.score = *p.Score
		}
		for _, f := range ovReplay.ring {
			if f.tick+uint64(pre) >= ovTicks {
				ovReplayKeep(f)
				c.frames = append(c.frames, f)
			}
		}
		ovReplay.clips[p.ID] = c
		if len(c.frames) > 0 {
			return true, int(c.frames[0].tick), nil
		}
		return true, int(ovTicks + 1), nil
	case "clip_drop":
		if c == ovReplay.playing && c != nil {
			return true, -1, fmt.Errorf("cannot drop the playing clip")
		}
		ovReplayDrop(p.ID, "host")
	case "clip_ready":
		if c == nil || c.open || len(c.frames) == 0 {
			return true, 0, nil
		}
		return true, int(Btoi(ovReplayClipReady(c))), nil
	case "clip_close", "clip_score":
		if c == nil {
			return true, -1, fmt.Errorf("unknown clip %d", p.ID)
		}
		if p.Score != nil {
			c.score = *p.Score
		}
		ovReplaySetKiller(c, p.Killer)
		if p.Op == "clip_close" {
			c.open = false
			if p.From != nil {
				i := 0
				for i < len(c.frames) && c.frames[i].tick < *p.From {
					ovReplayRelease(c.frames[i])
					c.frames[i] = nil
					i++
				}
				c.frames = c.frames[i:]
			}
			path := make([]float32, 0, len(c.frames)*4)
			var last [4]float32
			for _, f := range c.frames {
				pose, ok := f.poses[c.victim]
				if ok && (c.victimID < 0 || pose.id == c.victimID) {
					last = pose.at
					path = append(path, pose.at[:]...)
				} else {
					// despawned or its slot dealt to another fighter: where it was last, not shown
					path = append(path, last[0], last[1], last[2], 0)
				}
			}
			kpath, act := ovReplayClipActivity(c)
			first := uint64(0)
			if len(c.frames) > 0 {
				first = c.frames[0].tick
			}
			ovPushEvent(map[string]interface{}{"e": "clip_closed", "id": c.id, "first_tick": first, "ticks": len(c.frames),
				"path": path, "kpath": kpath, "act": act})
			return true, len(c.frames), nil
		}
	case "replay_begin":
		if c == nil || c.open || len(c.frames) == 0 {
			return true, -1, fmt.Errorf("clip %d is not ready", p.ID)
		}
		if !ovReplayClipReady(c) {
			return true, -1, fmt.Errorf("clip %d textures are still being readied", p.ID)
		}
		ovReplayStopChannels()
		if !ovReplayIsPlaying() {
			ovReplayHoldLive()
		}
		ovReplay.playing, ovReplay.shown, ovReplay.between, ovReplay.begun = c, 0, 0, false
		c.seen = make([]bool, len(c.frames))
		return true, len(c.frames), nil
	case "replay_show":
		if c == nil || c != ovReplay.playing {
			return true, -1, fmt.Errorf("clip %d is not playing", p.ID)
		}
		if math.IsNaN(p.Tick) || math.IsInf(p.Tick, 0) || p.Tick < 0 || p.Tick >= float64(len(c.frames)) {
			return true, -1, fmt.Errorf("replay tick outside clip")
		}
		ovReplay.shown = int(p.Tick)
		ovReplay.between = float32(p.Tick - math.Floor(p.Tick))
		if !c.seen[ovReplay.shown] {
			ovReplayShowSounds(c.frames[ovReplay.shown], !ovReplay.begun)
			ovReplay.begun = true
			c.seen[ovReplay.shown] = true
		} else {
			ovReplay.repeatedShows++
		}
	case "replay_end":
		ovReplayEnd()
	case "replay_clear":
		ovReplayEnd()
		for id := range ovReplay.clips {
			ovReplayDrop(id, "host")
		}
		for len(ovReplay.ring) > 0 {
			ovReplayPopRing()
		}
	case "replay_stats":
		ovReplayStats()
	}
	return true, 0, nil
}

// ovReplayActNear: ik units round the victim or killer an effect counts as the shot's action
const ovReplayActNear = 200.0

// ...ik units an effect moves in a frame to count: one that comes on, or flies, is action
const ovReplayActMove = 2.0

// ovReplayClipActivity: what a host can cut and frame a clip's shot by - per frame the killer's floor
// x/depth/height/shown (as the victim's path; zeros before one is known) and the action: sounds started and the screen
// shaking or a super's freeze (the whole fight's), then the victim's own effects (helpers, explods, projectiles) and
// the killer's round the victim that came on or moved since the frame before (an animation is one effect: its
// identity) - apart, so a shot is not kept on by the killer's sparks as it fights on elsewhere. Read only; once a
// clip closes. The entry's owner is tested first, its look and image only for the victim's and killer's own, so the
// frame's other entries cost one field each. Env IKEMEN_REPLAY_ACT_FAST=0: every entry's look and image read first.
var ovReplayActFast = os.Getenv("IKEMEN_REPLAY_ACT_FAST") != "0"

func ovReplayClipActivity(c *ovReplayClip) ([]float32, []float32) {
	kpath := make([]float32, 0, len(c.frames)*4)
	act := make([]float32, 0, len(c.frames)*4)
	var vlast, klast [4]float32
	type was struct {
		frame   int
		x, y, z float32
	}
	seen := make(map[uintptr]was, 256)
	for i, f := range c.frames {
		if p, ok := f.poses[c.victim]; ok && (c.victimID < 0 || p.id == c.victimID) {
			vlast = p.at
		}
		if p, ok := f.poses[c.killer]; c.killer >= 0 && ok && (c.killerID < 0 || p.id == c.killerID) {
			klast = p.at
			kpath = append(kpath, p.at[:]...)
		} else {
			kpath = append(kpath, klast[0], klast[1], klast[2], 0)
		}
		// (round the victim only: a killer that fights on elsewhere is not this shot's action)
		near := [2]float32{vlast[0] - ovFloorX0, (vlast[1] - ovFloorD0) * ovDepthScale}
		var fx [2]int // the victim's, the killer's
		for _, e := range f.sprites {
			if !ovReplayActFast && (e.look == nil || e.sd().ovKind == 0) {
				continue
			}
			who := -1
			if c.victimID >= 0 && e.owner == c.victimID+1 {
				who = 0
			} else if c.killerID >= 0 && e.owner == c.killerID+1 {
				who = 1
			}
			if who < 0 {
				continue
			}
			if ovReplayActFast && (e.look == nil || e.sd().ovKind == 0) {
				continue // (the same test after the owner's - the frame's other entries cost one field)
			}
			v := e.sd()
			before, ok := seen[e.identity]
			seen[e.identity] = was{i + 1, v.ovX, v.ovY, v.ovZ}
			if ok && before.frame == i && i > 0 && Abs(v.ovX-before.x) < ovReplayActMove && Abs(v.ovY-before.y) < ovReplayActMove &&
				Abs(v.ovZ-before.z) < ovReplayActMove {
				continue // (where it was the frame before: an effect looping in place is no action)
			}
			if vlast != ([4]float32{}) && Abs(v.ovX-near[0]) <= ovReplayActNear && Abs(v.ovZ-near[1]) <= ovReplayActNear {
				fx[who]++
			}
		}
		shake := float32(0)
		if f.shake.time > 0 || f.superTime > 0 {
			shake = 1
		}
		act = append(act, float32(len(f.sounds)), shake, float32(fx[0]), float32(fx[1]))
	}
	return kpath, act
}

// The clip's killer: the host's "k", else (once scored or closed) the victim's last attacker
func ovReplaySetKiller(c *ovReplayClip, k *int) {
	h := -1
	if k != nil {
		h = *k
	} else if c.killer < 0 && c.victim >= 0 && c.victim < len(ov.slots) {
		h = ov.slots[c.victim].lastHit
	}
	k2 := ovReplayLiveRoot(h)
	if k2 == nil || h == c.victim {
		return
	}
	if h != c.killer {
		c.killer, c.killerID, c.viewOK[1] = h, k2.id, false
	}
}

func ovReplayClipReady(c *ovReplayClip) bool {
	for _, f := range c.frames {
		for _, e := range f.sprites {
			if e.look == nil {
				continue
			}
			s := &e.look.asset.sprite
			if s.Tex == nil || (s.coldepth <= 8 && s.PalTex == nil) {
				return false
			}
		}
	}
	return true
}

func ovReplayHoldLive() {
	WithSpeakerLock(func() {
		for i := range sys.charSoundChannels {
			for j := range sys.charSoundChannels[i] {
				ch := &sys.charSoundChannels[i][j]
				if ch.ctrl != nil {
					ovReplay.livePaused[ch] = ch.ctrl.Paused
					ch.ctrl.Paused = true
				}
			}
		}
		for i := range sys.soundChannels {
			ch := &sys.soundChannels[i]
			if ch.ctrl != nil {
				ovReplay.livePaused[ch] = ch.ctrl.Paused
				ch.ctrl.Paused = true
			}
		}
	})
}

func ovReplayStopChannels() {
	ovReplayAudioWriting = true
	defer func() { ovReplayAudioWriting = false }()
	for id, ch := range ovReplay.channels {
		ch.Reset()
		delete(ovReplay.channels, id)
	}
}

func ovReplayEnd() {
	if !ovReplayIsPlaying() {
		return
	}

	ovReplayStopChannels()
	WithSpeakerLock(func() {
		for ch, paused := range ovReplay.livePaused {
			if ch.ctrl != nil {
				ch.ctrl.Paused = paused
			}
		}
	})
	ovReplay.livePaused = make(map[*SoundChannel]bool)
	ovReplay.playing = nil
	for i := range ovReplaySnap.ops { // (the replay snapshot's texture references go with the clip)
		ovReplaySnap.ops[i] = ovDrawOp{}
	}
	ovReplaySnap.ops = ovReplaySnap.ops[:0]
}

func ovReplayShowSounds(f *ovReplayFrame, first bool) {
	ovReplayAudioWriting = true
	defer func() { ovReplayAudioWriting = false }()
	for _, r := range f.sounds {
		ch := ovReplay.channels[r.id]
		if r.stop {
			if ch != nil {
				ch.Reset()
				delete(ovReplay.channels, r.id)
				ovReplay.audioStops++
			}
			continue
		}
		if r.play || (first && ch == nil) {
			if ch != nil {
				ch.Reset()
			}
			ch = &SoundChannel{}
			position, loop := r.start, r.loop
			if first && !r.play {
				position, loop = r.position, int32(r.remaining+1)
				if r.remaining < 0 {
					loop = -1
				}
			}
			func() {
				ovReplayAudioStart = &r
				defer func() { ovReplayAudioStart = nil }()
				ch.Play(r.sound, r.group, r.number, loop, Max(r.freq, 0.001), r.loopStart, r.loopEnd, position)
			}()
			ovReplay.channels[r.id] = ch
			ovReplay.audioPlays++
		}
		if ch != nil && ch.sfx != nil {
			ch.SetVolume(r.volume)
			ch.SetFreqMul(r.freq)
			ch.SetPaused(r.paused || r.freq == 0)
			gains := r.gains // (as it was heard live)
			WithSpeakerLock(func() { ovReplayPan.Store(ch.sfx, gains) })
		}
	}
}

// Temporarily present a frozen draw queue to exactly the same System.draw,
// Animation.Draw, RenderSprite and card-projection code as live rendering.
// The host camera is left alone. The Ikemen camera/math/shader clock is frozen.
func ovReplayRenderState(s *System) func() {
	if !ovReplayIsPlaying() {
		return func() {}
	}
	f := ovReplay.playing.frames[ovReplay.shown]
	list, cam, shake, fx := s.spriteList, s.cam, s.envShake, s.allPalFX
	superTime, superBright, tick, counter := s.supertime, s.superbrightness, s.tickCount, s.frameCounter
	brightness, brightnessOld := s.brightness, s.brightnessOld
	// (the frame's entries as whole SpriteData, drawn and interpolated in a scratch; what the draw wrote
	// into them goes back into the entries after it, as when it drew the entries themselves - ovReplayKeepDrawn)
	drawn := ovReplayDrawList(f)
	s.spriteList, s.cam, s.envShake, s.allPalFX = drawn, f.cam, f.shake, &f.allFX
	s.supertime, s.superbrightness, s.tickCount, s.frameCounter = f.superTime, f.superBrightness, f.gameTick, f.frameCounter
	// (entries are shared between frames - this frame's order is its list's; DrawList.draw assigns the
	// sync groups; both cleared again on restore)
	for i, sd := range drawn {
		sd.sortindex = i
	}
	var positions [][7]float32
	if ovReplay.between > 0 && ovReplay.shown+1 < len(ovReplay.playing.frames) {
		next := ovReplay.playing.frames[ovReplay.shown+1]
		positions = make([][7]float32, len(f.sprites))
		matches := make(map[uintptr]*ovReplaySprite, len(next.sprites))
		for _, r := range next.sprites {
			if _, exists := matches[r.identity]; exists {
				matches[r.identity] = nil
			} else {
				matches[r.identity] = r
			}
		}
		for i, r := range f.sprites {
			d := drawn[i]
			positions[i] = [7]float32{d.pos[0], d.pos[1], d.ovX, d.ovY, d.ovZ, d.ovRootZ, d.ovAnchorY}
			n := matches[r.identity]
			if n == nil || r.look == nil || n.look == nil || d.facing != n.sd().facing {
				continue
			}
			nd := n.sd() // (the next frame's entry, as recorded: one also in this frame mixes with itself - unchanged)
			// (its picture changed between the two ticks - a spin, a walk - but it is the same action: it moves on with
			// the rest. Held unless its picture was the same, a third of what moved stood still for the half tick and
			// then jumped the whole step, stuttering in slow motion)
			if r.look.asset.key != n.look.asset.key && d.ovAnimNo != nd.ovAnimNo {
				continue
			}
			if Abs(d.pos[0]-nd.pos[0]) > 256 || Abs(d.pos[1]-nd.pos[1]) > 256 {
				continue
			}
			mix := func(a, b float32) float32 { return a + (b-a)*ovReplay.between }
			d.pos = [2]float32{mix(d.pos[0], nd.pos[0]), mix(d.pos[1], nd.pos[1])}
			d.ovX, d.ovY, d.ovZ = mix(d.ovX, nd.ovX), mix(d.ovY, nd.ovY), mix(d.ovZ, nd.ovZ)
			d.ovRootZ = mix(d.ovRootZ, nd.ovRootZ) // (the depth order's: host_ov_depth.go)
			// (its card's anchor height with its drawn one: setSprite swaps the card's up for the world's by it - left at the
			// tick before, what flew was drawn off its path on every half tick and back on it the next)
			d.ovAnchorY = mix(d.ovAnchorY, nd.ovAnchorY)
		}
	}
	return func() {
		for i, e := range f.sprites { // (what the draw wrote stays in the entry, as when it drew the entry itself)
			ovReplayKeepDrawn(e, drawn[i])
		}
		for i, p := range positions {
			d := f.sprites[i].sd()
			d.pos = [2]float32{p[0], p[1]}
			d.ovX, d.ovY, d.ovZ, d.ovRootZ, d.ovAnchorY = p[2], p[3], p[4], p[5], p[6]
		}
		for _, e := range f.sprites {
			v := e.sd()
			v.sortindex, v.syncGroup = 0, 0
		}
		for i := range drawn { // (the scratch keeps no look alive)
			*drawn[i] = SpriteData{}
		}
		s.spriteList, s.cam, s.envShake, s.allPalFX = list, cam, shake, fx
		s.supertime, s.superbrightness, s.tickCount, s.frameCounter = superTime, superBright, tick, counter
		s.brightness, s.brightnessOld = brightness, brightnessOld
	}
}

// While the live match is still showing (pipeline off: the GL thread after a live frame; pipeline on: the host at
// step_end, the world parked, GL work queued for the GL thread). Immutable palette textures are readied a few at a
// time, avoiding first-clip upload stalls and shader hitches. Only the assets not readied yet are visited; an unready
// one is tried again later.
func ovReplayWarm() {
	if !ovReplay.enabled || ovReplayIsPlaying() || len(ovReplay.unwarmed) == 0 {
		return
	}
	deadline := time.Now().Add(700 * time.Microsecond)
	list := ovReplay.unwarmed
	keep := list[:0]
	i := 0
	for ; i < len(list); i++ {
		a := list[i]
		if a.refs == 0 {
			continue // (released: no frame draws it any more)
		}
		a.sprite.ovEnsureTex()
		if a.sprite.coldepth <= 8 && a.sprite.PalTex == nil {
			a.sprite.CachePalTex(a.sprite.Pal)
		}
		if a.sprite.Tex == nil || (a.sprite.coldepth <= 8 && a.sprite.PalTex == nil) {
			keep = append(keep, a) // (its source texels are not there yet)
		}
		if time.Now().After(deadline) {
			i++
			break
		}
	}
	keep = append(keep, list[i:]...)
	for j := len(keep); j < len(list); j++ {
		list[j] = nil
	}
	ovReplay.unwarmed = keep
}
