package tgbot

import (
	"sort"
	"sync"
	"time"

	tele "gopkg.in/telebot.v3"
)

type photoAlbumKey struct {
	userID   int64
	chatID   int64
	threadID int
	albumID  string
}

type photoAlbum struct {
	photos     map[int]tele.Context
	timer      *time.Timer
	generation uint64
}

// Telegram provides no album-complete event. Flush after a quiet interval,
// collecting before downloading so slow file requests cannot split an album.
type photoAlbumCollector struct {
	mu     sync.Mutex
	delay  time.Duration
	albums map[photoAlbumKey]*photoAlbum
}

func newPhotoAlbumCollector(delay time.Duration) *photoAlbumCollector {
	return &photoAlbumCollector{delay: delay, albums: make(map[photoAlbumKey]*photoAlbum)}
}

func (a *photoAlbumCollector) add(userID int64, c tele.Context, submit func([]tele.Context)) {
	key := photoAlbumKey{userID, c.Chat().ID, c.Message().ThreadID, c.Message().AlbumID}
	a.mu.Lock()
	defer a.mu.Unlock()
	album := a.albums[key]
	if album == nil {
		album = &photoAlbum{photos: make(map[int]tele.Context)}
		a.albums[key] = album
	}
	album.photos[c.Message().ID] = c
	if album.timer != nil {
		album.timer.Stop()
	}
	album.generation++
	generation := album.generation
	album.timer = time.AfterFunc(a.delay, func() {
		a.mu.Lock()
		if a.albums[key] != album || album.generation != generation {
			a.mu.Unlock()
			return
		}
		delete(a.albums, key)
		photos := make([]tele.Context, 0, len(album.photos))
		for _, photo := range album.photos {
			photos = append(photos, photo)
		}
		a.mu.Unlock()
		sort.Slice(photos, func(i, j int) bool { return photos[i].Message().ID < photos[j].Message().ID })
		submit(photos)
	})
}

func (a *photoAlbumCollector) cancel(userID, chatID int64, threadID int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, album := range a.albums {
		if key.userID == userID && key.chatID == chatID && key.threadID == threadID {
			album.timer.Stop()
			delete(a.albums, key)
		}
	}
}
