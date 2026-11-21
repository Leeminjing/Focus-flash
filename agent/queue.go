package agent

// TakeMode controls how many messages a queue yields at an execution checkpoint.
type TakeMode uint8

const (
	TakeOne TakeMode = iota
	TakeAll
)

// Each Agent owns a queue for each kind of input and serializes access to both.
type messageQueue struct {
	mode     TakeMode
	messages []string
}

func (q *messageQueue) push(text string) {
	q.messages = append(q.messages, text)
}

func (q *messageQueue) take() []string {
	if len(q.messages) == 0 {
		return nil
	}
	if q.mode == TakeAll || len(q.messages) == 1 {
		messages := q.messages
		q.messages = nil
		return messages
	}
	oldest := q.messages[0]
	q.messages[0] = ""
	q.messages = q.messages[1:]
	return []string{oldest}
}
