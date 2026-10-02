package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load()
	defaultPath := os.Getenv("DATABASE_PATH")
	if defaultPath == "" {
		defaultPath = "data/tg-gpt.db"
	}
	dbPath := flag.String("db", defaultPath, "Path to the conversation database (opened read-only)")
	list := flag.Bool("list", false, "List the 20 most recent saved conversations without calling the API")
	userID := flag.Int64("user-id", 0, "Telegram user ID from the conversation list")
	dialogID := flag.Int64("dialog-id", 0, "Conversation/topic ID (0 is the main chat)")
	model := flag.String("model", "", "Override the configured summarizer model")
	flag.Parse()
	selected := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "dialog-id" {
			selected = true
		}
	})
	if !*list && (*userID != 0) != selected {
		return errors.New("provide both -user-id and -dialog-id, or use -list")
	}
	absolutePath, err := filepath.Abs(*dbPath)
	if err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	db := &database.DB{DB: sqlDB}
	if *list || !selected {
		return listDialogs(db)
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("OPENAI_API_KEY is required; set it in the environment or .env")
	}
	if *model == "" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		*model = cfg.Memory.Extractor.Model
	}
	events, err := repositories.NewTraceRepo(db).GetAllForDialog(*userID, *dialogID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return errors.New("no events found for this user and dialog")
	}
	images := countImages(events)
	fmt.Fprintf(os.Stderr, "Summarizing %d events with %d images using %s (database is read-only)\n", len(events), images, *model)
	client := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	summary, err := services.NewSummarizer(&client, *model).Summarize(context.Background(), events)
	if err != nil {
		return err
	}
	if summary == "" {
		return errors.New("summarizer returned an empty summary")
	}
	fmt.Println(summary)
	return nil
}

func listDialogs(db *database.DB) error {
	rows, err := db.Query(`SELECT user_id, dialog_id, COUNT(*),
 SUM((SELECT COUNT(*) FROM json_each(trace_events.payload, '$.multi_content') WHERE json_extract(value, '$.type') = 'image_url'))
 FROM trace_events GROUP BY user_id, dialog_id ORDER BY MAX(created_at) DESC, MAX(id) DESC LIMIT 20`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Println("USER_ID\tDIALOG_ID\tEVENTS\tIMAGES")
	for rows.Next() {
		var userID, dialogID, events, images int64
		if err := rows.Scan(&userID, &dialogID, &events, &images); err != nil {
			return err
		}
		fmt.Printf("%d\t%d\t%d\t%d\n", userID, dialogID, events, images)
	}
	return rows.Err()
}

func countImages(events []models.TraceEvent) int {
	count := 0
	for _, event := range events {
		if event.EventType != models.EventTypeUserMsg {
			continue
		}
		var payload models.UserMsgPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		for _, part := range payload.MultiContent {
			if part.Type == llm.ContentPartImageURL && part.ImageURL != "" {
				count++
			}
		}
	}
	return count
}
