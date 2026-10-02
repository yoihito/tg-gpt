package tgbot

import (
	"reflect"
	"testing"
	"time"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

func TestVisionMessage(t *testing.T) {
	for _, caption := range []string{"Compare the images", "", "  "} {
		t.Run(caption, func(t *testing.T) {
			msg := llmVisionMessage(caption, "image1", "image2")
			wantCaption := caption
			if caption == "" || caption == "  " {
				wantCaption = "Please analyze the provided images."
			}
			want := llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: wantCaption},
				{Type: llm.ContentPartImageURL, ImageURL: "image1"},
				{Type: llm.ContentPartImageURL, ImageURL: "image2"},
			}}
			if !reflect.DeepEqual(msg, want) {
				t.Fatalf("got %#v, want %#v", msg, want)
			}
		})
	}
}

func photoContext(id int, chatID int64, threadID int, albumID string) tele.Context {
	return (&tele.Bot{}).NewContext(tele.Update{Message: &tele.Message{
		ID: id, Chat: &tele.Chat{ID: chatID}, ThreadID: threadID, AlbumID: albumID,
		Photo: &tele.Photo{},
	}})
}

func TestPhotoAlbumsCollectAndSort(t *testing.T) {
	collector := newPhotoAlbumCollector(20 * time.Millisecond)
	results := make(chan []tele.Context, 4)
	submit := func(photos []tele.Context) { results <- photos }
	collector.add(1, photoContext(3, 10, 1, "album"), submit)
	collector.add(1, photoContext(1, 10, 1, "album"), submit)
	collector.add(1, photoContext(1, 10, 1, "album"), submit)
	collector.add(1, photoContext(2, 10, 1, "album"), submit)
	select {
	case photos := <-results:
		if len(photos) != 3 {
			t.Fatalf("got %d photos", len(photos))
		}
		for i, photo := range photos {
			if photo.Message().ID != i+1 {
				t.Fatal("photos out of order")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("album never submitted")
	}
	select {
	case <-results:
		t.Fatal("album submitted twice")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPhotoAlbumsIsolationAndCancellation(t *testing.T) {
	collector := newPhotoAlbumCollector(20 * time.Millisecond)
	results := make(chan []tele.Context, 4)
	submit := func(photos []tele.Context) { results <- photos }
	collector.add(1, photoContext(1, 10, 1, "album"), submit)
	collector.add(1, photoContext(2, 10, 2, "album"), submit)
	collector.add(2, photoContext(3, 10, 1, "album"), submit)
	collector.add(1, photoContext(4, 11, 1, "album"), submit)
	collector.cancel(1, 10, 1)
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		select {
		case photos := <-results:
			if len(photos) != 1 {
				t.Fatal("unrelated albums combined")
			}
			seen[photos[0].Message().ID] = true
		case <-time.After(time.Second):
			t.Fatal("album never submitted")
		}
	}
	if !reflect.DeepEqual(seen, map[int]bool{2: true, 3: true, 4: true}) {
		t.Fatalf("unexpected albums: %v", seen)
	}
	select {
	case <-results:
		t.Fatal("cancelled album submitted")
	case <-time.After(50 * time.Millisecond):
	}
}
