package base

type RelationType string

var (
	Output       RelationType = "output"
	Input        RelationType = "input"
	Intermediate RelationType = ""
)

type Base struct{}

func (writer *Base) Symbol(value string) Symbol {
	return Symbol(value)
}

func (writer *Base) Unsigned(value uint32) Unsigned {
	return Unsigned(value)
}

func (writer *Base) Record(elements ...Element) Record {
	return Record(elements)
}