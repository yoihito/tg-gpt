package telegram_utils

import (
	"log"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/handlers"
)

const MAX_TELEGRAM_MESSAGE_LENGTH = 4096
const STREAMING_INTERVAL = 200

type Commands struct {
	Command string
	Content string
	Err     error
}

func ShapeStream(messagesCh <-chan handlers.Result) <-chan Commands {
	commandsCh := make(chan Commands)
	go func() {
		defer close(commandsCh)
		prevLength := 0
		accumulatedMessage := ""
		editing := false
		for message := range messagesCh {
			if message.Err != nil {
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
			if len(accumulatedMessage)-prevLength < STREAMING_INTERVAL && message.Status != handlers.EOF_STATUS {
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

func SendStream(c tele.Context, replyTo *tele.Message, chunksCh <-chan handlers.Result) error {
	commandsCh := ShapeStream(chunksCh)
	var currentMessage *tele.Message
	var err error
	for command := range commandsCh {
		log.Printf("Command: %+v\n", command)
		if command.Err != nil {
			return command.Err
		}
		if command.Command == "start" {
			currentMessage, err = c.Bot().Reply(replyTo, command.Content)
		} else if command.Command == "edit" {
			_, err = c.Bot().Edit(currentMessage, command.Content)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
