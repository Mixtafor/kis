package news

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestScore(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		sources []Contribution
		half    int64
		want    float64
	}{
		{"empty", nil, 21600, 0},
		{"staff", []Contribution{{Trust: 1, EventAt: now}}, 21600, 1},
		{"three freelance", []Contribution{{Trust: .25, EventAt: now}, {Trust: .35, EventAt: now}, {Trust: .2, EventAt: now}}, 21600, .8},
		{"half life", []Contribution{{Trust: 1, EventAt: now.Add(-6 * time.Hour)}}, 21600, .5},
		{"two half lives", []Contribution{{Trust: .8, EventAt: now.Add(-12 * time.Hour)}}, 21600, .2},
		{"year old rumor", []Contribution{{Trust: 1, EventAt: now.Add(-365 * 24 * time.Hour)}}, 21600, 0},
		{"clock skew clamped", []Contribution{{Trust: .35, EventAt: now.Add(time.Minute)}}, 21600, .35},
		{"invalid divisor", []Contribution{{Trust: 1, EventAt: now}}, 0, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := Score(tt.sources, now, tt.half)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("score=%g want%g", got, tt.want)
			}
		})
	}
}

func TestScoreOnlyDecays(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	items := []Contribution{{Trust: .35, EventAt: now.Add(-time.Hour)}, {Trust: 1, EventAt: now}}
	previous := Score(items, now, 21600)
	for day := 1; day <= 365; day++ {
		next := Score(items, now.Add(time.Duration(day)*24*time.Hour), 21600)
		if next > previous {
			t.Fatal("trust increased without an incoming contribution")
		}
		previous = next
	}
}

func TestSubmissionValidation(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	valid := Submission{RequestID: "request-1", Category: "Город", Topic: "Мост открыт", Body: "Описание", EventAt: now}
	cases := []struct {
		name      string
		change    func(*Submission)
		wantValid bool
	}{
		{"valid", func(*Submission) {}, true},
		{"old accepted", func(n *Submission) { n.EventAt = now.Add(-365 * 24 * time.Hour) }, true},
		{"exact skew boundary", func(n *Submission) { n.EventAt = now.Add(time.Minute) }, true},
		{"future rejected", func(n *Submission) { n.EventAt = now.Add(time.Minute + time.Nanosecond) }, false},
		{"zero timestamp", func(n *Submission) { n.EventAt = time.Time{} }, false},
		{"empty topic", func(n *Submission) { n.Topic = " \n\t" }, false},
		{"null byte", func(n *Submission) { n.Category = "city\x00" }, false},
		{"invalid utf8", func(n *Submission) { n.Body = string([]byte{255}) }, false},
		{"missing id", func(n *Submission) { n.RequestID = "" }, false},
		{"maximum body", func(n *Submission) { n.Body = strings.Repeat("x", 10000) }, true},
		{"body too long", func(n *Submission) { n.Body = strings.Repeat("x", 10001) }, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			n := valid
			tt.change(&n)
			if (n.Validate(now) == nil) != tt.wantValid {
				t.Fatalf("validation=%v", n.Validate(now))
			}
		})
	}
	before := valid
	if valid.Validate(now) != nil || valid != before {
		t.Fatal("validation must preserve the exact story key")
	}
}

func TestPolicyValidation(t *testing.T) {
	for _, tt := range []struct {
		p     Policy
		valid bool
	}{
		{Policy{1.1, 21600, 3}, true}, {Policy{.1, 1, 0}, true},
		{Policy{0, 21600, 3}, false}, {Policy{math.NaN(), 21600, 3}, false},
		{Policy{math.Inf(1), 21600, 3}, false}, {Policy{1, 0, 3}, false},
		{Policy{1, 31536001, 3}, false}, {Policy{1, 21600, -1}, false},
		{Policy{1, 21600, 1001}, false},
	} {
		if (tt.p.Validate() == nil) != tt.valid {
			t.Fatalf("policy=%+v", tt.p)
		}
	}
}

func TestClientValidation(t *testing.T) {
	for _, tt := range []struct {
		role  string
		trust float64
		valid bool
	}{
		{"staff", 1, true}, {"outstaff", .35, true}, {"editor", 0, true},
		{"freelancer", .35, false},
		{"unknown", .5, false}, {"staff", -1, false}, {"staff", 1.01, false},
		{"staff", math.NaN(), false}, {"staff", math.Inf(1), false},
	} {
		c := Client{ID: "source", Name: "Source", Role: tt.role, Trust: tt.trust}
		if (c.Validate() == nil) != tt.valid {
			t.Fatalf("client=%+v", c)
		}
	}
}

// Проверяем один вклад от автора, порог публикации и поздравления по порядку.
func TestAdmission(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p := Policy{Threshold: 1.1, HalfLifeSeconds: 21600, CongratulationsCount: 1}
	first := Contribution{ClientID: "first", Name: "First", Trust: .7, EventAt: now, ReceivedAt: now, Rank: 1}
	n := Submission{EventAt: now}
	for _, tt := range []struct {
		name, id              string
		weight                float64
		event                 time.Time
		published             bool
		wantVote, wantPublish bool
		wantScore             float64
	}{
		{"independent source", "second", .5, now, false, true, true, 1.2},
		{"same source one vote", "first", 1, now, false, false, false, .7},
		{"old source not revived", "second", .5, now.Add(-365 * 24 * time.Hour), false, true, false, .7},
		{"published no credit", "second", 1, now, true, false, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n.EventAt = tt.event
			original := []Contribution{first}
			d := Assess(p, original, Client{ID: tt.id, Name: "source", Trust: tt.weight}, n, now, tt.published)
			if (d.Contribution != nil) != tt.wantVote || d.Publish != tt.wantPublish || math.Abs(d.Trust-tt.wantScore) > 1e-12 {
				t.Fatalf("decision=%+v", d)
			}
			if original[0] != first {
				t.Fatal("mutated existing contribution")
			}
			if d.Publish && (len(d.Congratulations) != 1 || d.Congratulations[0].ClientID != "first") {
				t.Fatal("congratulations must follow receipt rank")
			}
		})
	}
}
