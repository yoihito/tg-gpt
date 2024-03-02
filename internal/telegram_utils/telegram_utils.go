package telegram_utils

import "vadimgribanov.com/tg-gpt/internal/handlers"

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
