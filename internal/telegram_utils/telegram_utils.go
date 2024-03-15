package telegram_utils

import (
	"log"
	"strings"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/services"
)

const MAX_TELEGRAM_MESSAGE_LENGTH = 4096
const STREAMING_INTERVAL = 200

type Commands struct {
	Command string
	Content string
	Err     error
}

func ShapeStream(messagesCh <-chan services.Result) <-chan Commands {
	commandsCh := make(chan Commands)
	go func() {
		defer close(commandsCh)
		prevLength := 0
		accumulatedMessage := ""
		editing := false
		for message := range messagesCh {
			if message.Err != nil {
				log.Println(message.Err)
				commandsCh <- Commands{Err: message.Err}
				return
			}
			if len(accumulatedMessage)+len(message.TextChunk) >= MAX_TELEGRAM_MESSAGE_LENGTH {
				if prevLength != len(accumulatedMessage) {
					commandsCh <- Commands{Command: "edit", Content: accumulatedMessage}
				}
				prevLength = 0
				accumulatedMessage = ""
				editing = false
			}

			accumulatedMessage += message.TextChunk
			if len(accumulatedMessage)-prevLength < STREAMING_INTERVAL && message.Status != services.EOF_STATUS {
				continue
			}
			prevLength = len(accumulatedMessage)
			if !editing {
				editing = true
				commandsCh <- Commands{Command: "start", Content: accumulatedMessage}
			} else {
				commandsCh <- Commands{Command: "edit", Content: accumulatedMessage}
			}
		}
	}()
	return commandsCh
}

func SendStream(c tele.Context, replyTo *tele.Message, chunksCh <-chan services.Result) error {
	commandsCh := ShapeStream(chunksCh)
	var currentMessage *tele.Message
	var err error
	for command := range commandsCh {
		log.Printf("Command: %+v\n", command)
		if command.Err != nil {
			log.Println(command.Err)
			return command.Err
		}
		if command.Command == "start" {
			currentMessage, err = c.Bot().Reply(replyTo, FixMarkdown(command.Content), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
			if err != nil {
				currentMessage, err = c.Bot().Reply(replyTo, command.Content, &tele.SendOptions{ParseMode: tele.ModeDefault})
				log.Println("Retry error", err)
			}
		} else if command.Command == "edit" {
			_, err = c.Bot().Edit(currentMessage, FixMarkdown(command.Content), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
			if err != nil {
				_, err = c.Bot().Edit(currentMessage, command.Content, &tele.SendOptions{ParseMode: tele.ModeDefault})
				log.Println("Retry error", err)
			}
		}
		if err != nil {
			log.Println("Error stream:", err)
			return err
		}
	}
	return nil
}

func GetUnclosedTag(markdown string) string {
	// order is important!
	var tags = []string{
		"```",
		"`",
		"*",
		"_",
	}
	var currentTag = ""

	markdownRunes := []rune(markdown)

	var i = 0
outer:
	for i < len(markdownRunes) {
		// skip escaped characters (only outside tags)
		if markdownRunes[i] == '\\' && currentTag == "" {
			i += 2
			continue
		}
		if currentTag != "" {
			if strings.HasPrefix(string(markdownRunes[i:]), currentTag) {
				// turn a tag off
				i += len(currentTag)
				currentTag = ""
				continue
			}
		} else {
			for _, tag := range tags {
				if strings.HasPrefix(string(markdownRunes[i:]), tag) {
					// turn a tag on
					currentTag = tag
					i += len(currentTag)
					continue outer
				}
			}
		}
		i++
	}

	return currentTag
}
func IsValid(markdown string) bool {
	return GetUnclosedTag(markdown) == ""
}

func FixMarkdown(markdown string) string {
	tag := GetUnclosedTag(markdown)
	if tag == "" {
		return markdown
	}
	return markdown + tag
}
