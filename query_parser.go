package graphql

import (
	"errors"
)

var (
	ErrorUnexpectedEOF = errors.New("unexpected EOF")
)

type Operator struct {
	operationType string // "query" || "mutation" || "subscription"
	name          string // "" = no name given
}

type Field struct {
	alias string
	name  string
}

func ParseQuery(input string) (*Operator, error) {
	iter := &Iter{
		data: input,
	}

	res, err := iter.parseOperator()
	if err != nil {
		return nil, err
	}

	return res, nil
}

type Iter struct {
	data   string
	charNr uint64
}

func (i *Iter) checkC(nr uint64) (res rune, end bool) {
	if i.eof(nr) {
		return 0, true
	}
	return i.c(nr), false
}

func (i *Iter) c(nr uint64) rune {
	return rune(i.data[nr])
}

func (i *Iter) eof(nr uint64) bool {
	return nr >= uint64(len(i.data))
}

func (i *Iter) currentC() rune {
	return i.c(i.charNr)
}

// https://spec.graphql.org/June2018/#sec-Language.Operations
func (i *Iter) parseOperator() (*Operator, error) {
	res := Operator{
		operationType: "query",
		name:          "",
	}

	stage := "operationType"

	for {
		if i.eof(i.charNr) {
			if stage != "operationType" {
				return nil, ErrorUnexpectedEOF
			}
			return &res, nil
		}

		if i.isIgnoredToken() {
			i.charNr++
			continue
		}

		c := i.currentC()
		switch stage {
		case "operationType":
			switch c {
			// For making a query you don't have to define a stage
			case '{':
				stage = "selectionSets"
			default:
				newOperationType := i.matches("query", "mutation", "subscription")
				if len(newOperationType) > 0 {
					res.operationType = newOperationType
					stage = "name"
				} else {
					return nil, errors.New("unknown operation type")
				}
			}
		case "name":
			switch c {
			case '(':
				stage = "variableDefinitions"
			case '@':
				stage = "directives"
			case '{':
				stage = "selectionSets"
			default:
				res.name = i.parseName()
				stage = "variableDefinitions"
			}
		case "variableDefinitions":
			switch c {
			case '@':
				stage = "directives"
			case '{':
				stage = "selectionSets"
			default:
				// TODO: https://spec.graphql.org/June2018/#VariableDefinitions
				return nil, errors.New("https://spec.graphql.org/June2018/#VariableDefinitions")
			}
		case "directives":
			switch c {
			case '{':
				stage = "selectionSets"
			default:
				// TODO: https://spec.graphql.org/June2018/#sec-Language.Directives
				return nil, errors.New("https://spec.graphql.org/June2018/#sec-Language.Directives")
			}
		case "selectionSets":
			err := i.parseSelectionSets()
			if err != nil {
				return nil, err
			}
			return &res, nil
		}

		i.charNr++
	}
}

// https://spec.graphql.org/June2018/#sec-Selection-Sets
func (i *Iter) parseSelectionSets() error {
	for {
		c, eof := i.checkC(i.charNr)
		if eof {
			return ErrorUnexpectedEOF
		}

		if i.isIgnoredToken() {
			i.charNr++
			continue
		}

		if c == '}' {
			return nil
		}

		err := i.parseSelection()
		if err != nil {
			return err
		}

		if c == '}' {
			return nil
		}

		i.charNr++
	}
}

// https://spec.graphql.org/June2018/#Selection
func (i *Iter) parseSelection() error {

	if len(i.matches("...")) > 0 {
		i.charNr++
		// TODO data is:
		// - FragmentSpread
		// - InlineFragment
		return errors.New("https://spec.graphql.org/June2018/#Selection")
	} else {
		i.parseField()
	}

	return nil
}

// https://spec.graphql.org/June2018/#Field
func (i *Iter) parseField() (*Field, error) {
	res := Field{}
	nameOrAlias := i.parseName()
	if nameOrAlias == "" {
		return nil, errors.New("field should have a name")
	}

	for {
		c, eof := i.checkC(i.charNr)
		if eof {
			return nil, ErrorUnexpectedEOF
		}

		if i.isIgnoredToken() {
			i.charNr++
			continue
		}

		if c == ':' {
			res.alias = nameOrAlias

		} else {

		}
		break
	}

	// TODO Order:
	// Alias (opt)
	// Name
	// Arguments (opt)
	// Directives (opt)
	// SelectionSet (opt)

	return &res, nil
}

// https://spec.graphql.org/June2018/#Name
func (i *Iter) parseName() string {
	allowedChars := map[rune]bool{}
	for _, allowedChar := range []byte("abcdefghijklmnopqrstuvwxyz_") {
		allowedChars[rune(allowedChar)] = true
	}
	for _, notFirstAllowedChar := range []byte("0123456789") {
		allowedChars[rune(notFirstAllowedChar)] = false
	}

	name := ""
	for {
		c, eof := i.checkC(i.charNr)
		if eof {
			return name
		}

		allowedAsFirstChar, ok := allowedChars[c]
		if !ok {
			return name
		}

		if name == "" && !allowedAsFirstChar {
			return name
		}

		name += string(c)

		i.charNr++
	}
}

// https://spec.graphql.org/June2018/#sec-Source-Text.Ignored-Tokens
func (i *Iter) isIgnoredToken() bool {
	c := i.currentC()
	return isUnicodeBom(c) || isWhiteSpace(c) || i.isLineTerminator() || i.isComment(true)
}

func (i *Iter) MustIgnoreNextTokens() error {
	for {
		c, eof := i.checkC(i.charNr)
		if eof {
			return c.isIgnoredToken()
		}
	}
}

// https://spec.graphql.org/June2018/#UnicodeBOM
func isUnicodeBom(input rune) bool {
	return input == '\uFEFF'
}

// https://spec.graphql.org/June2018/#WhiteSpace
func isWhiteSpace(input rune) bool {
	return input == ' ' || input == '\t'
}

// https://spec.graphql.org/June2018/#LineTerminator
func (i *Iter) isLineTerminator() bool {
	c := i.currentC()
	if c == '\n' {
		return true
	}
	if c == '\r' {
		next, _ := i.checkC(i.charNr + 1)
		if next == '\n' {
			i.charNr++
		}
		return true
	}
	return false
}

// https://spec.graphql.org/June2018/#Comment
func (i *Iter) isComment(parseComment bool) bool {
	if i.currentC() == '#' {
		if parseComment {
			i.parseComment()
		}
		return true
	}
	return false
}

func (i *Iter) parseComment() {
	for {
		if i.eof(i.charNr) {
			return
		}
		if i.isLineTerminator() {
			return
		}
		i.charNr++
	}
}

func (i *Iter) matches(oneOf ...string) string {
	startIdx := i.charNr

	oneOfMap := map[string]bool{}
	for _, val := range oneOf {
		oneOfMap[val] = true
	}

	for {
		c, eof := i.checkC(i.charNr)
		if eof {
			i.charNr = startIdx
			return ""
		}
		offset := i.charNr - startIdx

		for key := range oneOfMap {
			keyLen := uint64(len(key))
			if offset >= keyLen || rune(key[offset]) != c {
				delete(oneOfMap, key)
			}
			if keyLen == offset+1 {
				return key
			}
		}

		if len(oneOfMap) == 0 {
			i.charNr = startIdx
			return ""
		}

		i.charNr++
	}

}
