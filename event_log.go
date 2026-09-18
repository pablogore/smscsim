package main

import "log"

// logEvent is the simulator's default subscriber. The log is no longer where the simulator records
// what it did; it is one view of the event stream, registered like any other.
//
// Text already carries exactly what the corresponding log.Printf produced, including its trailing
// newline where it had one, and log.Print adds a newline only when the line lacks it. That is what
// keeps the simulator's output byte for byte what it always was.
func logEvent(e Event) {
	if e.Text == "" {
		return
	}
	log.Print(e.Text)
}
