package redact

import (
	"regexp"
	"strings"
	"sync"
)

// nameDetector finds person names with a gazetteer of given names plus context
// rules.
//
// Be clear about what this can and cannot do. Names are the one entity type
// here with no internal structure to validate against: "Mark" is a name and a
// verb, "Rose" is a name and a flower, "Paris Hilton" is two place names.
// Without a sequence model the achievable recall on ordinary prose is
// somewhere around a half -- the fixture corpus measures it, and the number is
// asserted in eval_test.go rather than claimed here -- and the misses are
// systematic: surnames standing alone, given names outside the gazetteer, and
// names in languages the list does not cover.
//
// Two rules therefore drive it, with different confidences:
//
//   - A capitalised word in the gazetteer, optionally followed by another
//     capitalised word taken as a surname. Confidence 0.5: this is the rule
//     that produces the false positives.
//   - A capitalised word preceded by an honorific or an introducing phrase
//     ("my name is", "Dear", "Sincerely,"). Confidence 0.85, and it works for
//     names the gazetteer has never seen, which is where most of the useful
//     recall comes from.
//
// A deployment that needs better than this should put a named-entity model
// behind the Detector interface. The interface exists partly so that swapping
// this out does not touch anything else.
type nameDetector struct{}

// NewPersonNameDetector detects person names.
func NewPersonNameDetector() Detector { return nameDetector{} }

func (nameDetector) Type() EntityType { return EntityPersonName }

// MaxLen covers "Firstname Middlename Lastname" comfortably. It bounds the
// streaming look-behind, so it is deliberately not generous.
func (nameDetector) MaxLen() int   { return 64 }
func (nameDetector) Priority() int { return priorityName }

var (
	capitalisedWord = regexp.MustCompile(`\b[A-Z][a-z]+(?:['\-][A-Z]?[a-z]+)*\b`)
	honorific       = regexp.MustCompile(`(?i)\b(?:mr|mrs|ms|miss|mx|dr|prof|professor|sir|dame|rev)\.?\s+$`)
	introducer      = regexp.MustCompile(`(?i)(?:my name is|i am|name:|names?:|signed[,:]?|sincerely[,:]?|regards[,:]?|best[,:]?|dear|from:|to:|attn:|contact|customer|patient|client|employee)\s+$`)
)

func (nameDetector) Find(text string) []Match {
	words := capitalisedWord.FindAllStringIndex(text, -1)
	if words == nil {
		return nil
	}
	gaz := gazetteer()

	var out []Match
	for i := 0; i < len(words); i++ {
		start, end := words[i][0], words[i][1]
		word := text[start:end]

		lower := strings.ToLower(word)
		if honorificWords[lower] {
			// "Dear Mr. Thompson" cues on "Dear" and would otherwise redact
			// "Mr". The honorific is not the name and carries none of the
			// identifying information.
			continue
		}

		before := text[:start]
		cued := honorific.MatchString(before) || introducer.MatchString(before)
		known := gaz[lower]
		if !cued && !known {
			continue
		}
		if !cued && commonWordsThatAreAlsoNames[lower] && !followedByCapital(text, words, i) {
			// "Mark the item as read" should not redact "Mark". A following
			// capitalised word is weak evidence of a full name and is the only
			// thing that rescues these; on its own, an ambiguous given name in
			// mid-sentence is more often the ordinary word.
			continue
		}

		conf := 0.5
		if cued {
			conf = 0.85
		}
		// Absorb one following capitalised word as a surname when it directly
		// follows, separated by a single space.
		if i+1 < len(words) && words[i+1][0] == end+1 && text[end] == ' ' {
			if !sentenceStart(text, words[i+1][0]) {
				end = words[i+1][1]
				i++
				conf += 0.1
			}
		}

		out = append(out, Match{
			Type: EntityPersonName, Start: start, End: end,
			Value: text[start:end], Confidence: conf,
		})
	}
	return out
}

func followedByCapital(text string, words [][]int, i int) bool {
	return i+1 < len(words) && words[i+1][0] == words[i][1]+1 && text[words[i][1]] == ' '
}

// sentenceStart reports whether the capitalised word at pos is capitalised only
// because a sentence begins there.
func sentenceStart(text string, pos int) bool {
	for i := pos - 1; i >= 0; i-- {
		switch text[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '.', '!', '?':
			return true
		default:
			return false
		}
	}
	return true
}

// honorificWords are titles that precede a name and are never the name.
var honorificWords = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "miss": true, "mx": true, "dr": true,
	"prof": true, "professor": true, "sir": true, "dame": true, "rev": true,
	"madam": true, "lord": true, "lady": true,
}

// commonWordsThatAreAlsoNames are gazetteer entries that are ordinary English
// words. They are the largest source of false positives, so they need the extra
// evidence of a following surname before they count.
var commonWordsThatAreAlsoNames = map[string]bool{
	"mark": true, "rose": true, "bill": true, "will": true, "grace": true,
	"april": true, "may": true, "june": true, "august": true, "art": true,
	"frank": true, "hope": true, "joy": true, "faith": true, "victor": true,
	"summer": true, "dawn": true, "sky": true, "ray": true, "rich": true,
	"jack": true, "guy": true, "max": true, "don": true, "pat": true,
}

// gazetteer is the given-name list, lowercased. It is small on purpose: every
// entry is a potential false positive, and the long tail of rare given names
// contributes little recall while adding a great deal of noise. Extending it is
// a deployment decision, which is why the type is a detector rather than a
// package-level list anyone can append to.
var gazetteer = sync.OnceValue(func() map[string]bool {
	m := make(map[string]bool, 512)
	for _, n := range strings.Fields(givenNames) {
		m[n] = true
	}
	return m
})

const givenNames = `
aaron abigail adam adrian agnes ahmed aisha alan albert alejandro alex alexander
alexandra alice alicia alison amanda amelia amir amy ana anders andrea andrew
angela anita ann anna anne annie anthony antonio april arthur ashley august
barbara beatrice ben benjamin bernard beth betty bill bob bradley brandon brenda
brian bruce bryan camila carl carlos carmen carol caroline carolyn catherine
cathy cecilia charles charlotte cheryl chloe chris christian christina christine
christopher claire clara claudia colin connie craig cristina cynthia daniel
danielle darren dave david dawn dean deborah debra denis denise dennis diana
diane diego dmitri dominic don donald donna dorothy douglas dylan edward eileen
elaine eleanor elena elias elisabeth elizabeth ella ellen emily emma eric erica
erik ernest esther eugene eva evelyn faith fatima felix fernando fiona frances
francis frank franklin fred frederick gabriel gail gary george georgia gerald
gina giovanni gloria grace graham greg gregory guy hannah hans harold harry
heather hector helen henry hiroshi hope howard hugo ian ibrahim ingrid irene
isabel isabella ivan jack jackie jacob jacqueline james jamie jan jane janet
janice jason javier jean jeff jeffrey jennifer jeremy jerry jesse jessica jesus
jill jim joan joanna joe john johnny jonathan jordan jorge jose joseph joshua
joy juan judith judy julia julian julie justin karen karl kate katherine kathleen
kathy keith kelly ken kenneth kevin kim kimberly kirsten klaus kristen kyle lars
laura lauren laurence lawrence lee leo leonard leslie liam lillian linda lisa
liu logan lois lorraine louis louise lucas lucia lucy luis luke lydia lynn magnus
malik marc marcus margaret maria marie marilyn mario marion mark marta martha
martin mary mateo matthew maureen maurice max maya megan mehmet melissa michael
michelle miguel mikael mike mikhail miranda mohammed monica nancy naomi natalie
nathan neil nicholas nicola nicole nils nina noah norma olga oliver olivia omar
oscar pablo pamela patricia patrick paul paula pauline pedro peggy peter philip
phillip phyllis pierre priya rachel rafael ralph ramon randy raul ray raymond
rebecca renee ricardo richard rita robert roberta roberto robin rodney roger
roland ron ronald rosa rose rosemary roy ruben ruth ryan sabrina sally salma
samantha samuel sandra sara sarah scott sean sebastian sergei shannon sharon
sheila shirley simon sofia sonia sophia sophie stanley stefan stephanie stephen
steve steven stuart susan suzanne sven sylvia tamara tara ted teresa terry
theodore theresa thomas tim timothy tina todd tom tomas tony tracy travis trevor
tyler ulrich valerie vanessa vera veronica victor victoria vincent violet
virginia vladimir walter wanda wayne wendy wesley wilhelm william willie wolfgang
xavier yolanda yuki yusuf yvonne zachary zoe
`
