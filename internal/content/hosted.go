package content

import "encoding/json"

// hostedDefaultBlocks is the starting page for a hosted event: a small website with About, date and time,
// a countdown, the programme, the venue details and registration. Every block is editable, removable and
// re-orderable in the editor; the ids match the setup targets BuildInitialContent fills in.
const hostedDefaultBlocks = `[
{"id":"hero","type":"hero","title":"Annual awards night","subtitle":"You're invited","image":null},
{"id":"about","type":"text","heading":"About","body":"Tell guests what this event is about, who it is for and why they should come."},
{"id":"when","type":"datetime","heading":"Date & time","start_local":"2027-06-12T18:00","end_local":"","timezone":"UTC","all_day":false},
{"id":"countdown","type":"countdown","heading":"Countdown"},
{"id":"programme","type":"schedule","heading":"Programme","items":[
  {"time":"18:00","title":"Doors open and registration","location":"","description":""},
  {"time":"19:00","title":"Welcome address","location":"","description":""},
  {"time":"19:30","title":"Main programme","location":"","description":""}]},
{"id":"where","type":"location","heading":"Event details","name":"The venue","address":"","map_url":"","notes":"Parking, accessibility and anything else guests should know."},
{"id":"rsvp","type":"rsvp","heading":"Register","body":"Register to get your entry pass.","deadline_local":"","capacity":null,"max_party_size":4,"fields":[{"key":"organisation","required":false},{"key":"dietary","required":false}],"questions":[]}
]`

// HostedOccasion returns occ with the hosted-event website sections as its default blocks. Only the default
// blocks change: the setup questions, RSVP fields and optional blocks stay the occasion's own.
func HostedOccasion(occ Occasion) Occasion {
	occ.DefaultBlocks = json.RawMessage(hostedDefaultBlocks)
	return occ
}
