// Package names generates session handles: Docker-style word pairs such as
// "eager_turing", an adjective and the surname of a well-known scientist or
// engineer. A handle is what chats store for a mention; it never changes, so
// it stays unambiguous when the session is renamed.
package names

import (
	"crypto/rand"
	"math/big"
	"regexp"
)

var adjectives = []string{
	"admiring", "agile", "amazing", "awesome", "blissful", "bold", "brave", "bright", "busy", "calm",
	"careful", "charming", "cheerful", "clever", "cool", "curious", "dazzling", "determined", "eager", "elastic",
	"elegant", "epic", "fervent", "festive", "focused", "friendly", "gallant", "gentle", "gifted", "glowing",
	"happy", "hopeful", "humble", "inspiring", "jolly", "keen", "kind", "laughing", "lucid", "magical",
	"modest", "musing", "mystifying", "nice", "nifty", "noble", "optimistic", "patient", "peaceful", "pensive",
	"playful", "practical", "priceless", "quick", "quirky", "relaxed", "romantic", "serene", "sharp", "sleepy",
	"stoic", "sunny", "swift", "tender", "thirsty", "tidy", "trusting", "upbeat", "vibrant", "vigilant",
	"vivid", "wise", "witty", "wonderful", "zealous", "zen",
}

var surnames = []string{
	"agnesi", "archimedes", "babbage", "banach", "bardeen", "bell", "bohr", "boole", "borg", "brahe",
	"cerf", "chandrasekhar", "curie", "darwin", "dijkstra", "einstein", "euclid", "euler", "faraday", "fermat",
	"fermi", "feynman", "franklin", "galileo", "gauss", "goldwasser", "goodall", "hamilton", "hawking", "heisenberg",
	"hertz", "hodgkin", "hopper", "hypatia", "johnson", "kepler", "kilby", "knuth", "kowalevski", "lamarr",
	"lamport", "leakey", "liskov", "lovelace", "maxwell", "mccarthy", "meitner", "mendel", "minsky", "mirzakhani",
	"newton", "nobel", "noether", "pascal", "pasteur", "planck", "poincare", "raman", "ramanujan", "ride",
	"ritchie", "rubin", "sagan", "shannon", "shaw", "sinoussi", "tesla", "thompson", "torvalds", "turing",
	"volta", "wilson", "wing", "wozniak", "wright", "yalow", "yonath",
}

// Pattern matches a handle; mentions are written @<handle>.
var Pattern = regexp.MustCompile(`^[a-z]+_[a-z]+(_[0-9]+)?$`)

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		panic(err)
	}
	return list[n.Int64()]
}

// New returns a random handle that taken does not report as used. After a
// few clashes a number is appended, so it always returns.
func New(taken func(string) bool) string {
	for i := 0; i < 50; i++ {
		h := pick(adjectives) + "_" + pick(surnames)
		if !taken(h) {
			return h
		}
	}
	base := pick(adjectives) + "_" + pick(surnames)
	for n := 2; ; n++ {
		h := base + "_" + itoa(n)
		if !taken(h) {
			return h
		}
	}
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }
