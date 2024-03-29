package goyaml

// #cgo LDFLAGS: -lm -lpthread
// #cgo CFLAGS: -I. -DHAVE_CONFIG_H=1
//
// #include "helpers.h"
import "C"

import (
	"reflect"
	"strconv"
	"unsafe"
)

const (
	documentNode = 1 << iota
	mappingNode
	sequenceNode
	scalarNode
	aliasNode
)

type node struct {
	kind         int
	line, column int
	tag          string
	value        string
	implicit     bool
	children     []*node
	anchors      map[string]*node
}

func stry(s *C.yaml_char_t) string {
	return C.GoString((*C.char)(unsafe.Pointer(s)))
}

// ----------------------------------------------------------------------------
// Parser, produces a node tree out of a libyaml event stream.

type parser struct {
	parser C.yaml_parser_t
	event  C.yaml_event_t
	doc    *node
}

func newParser(b []byte) *parser {
	p := parser{}
	if C.yaml_parser_initialize(&p.parser) == 0 {
		panic("Failed to initialize YAML emitter")
	}

	if len(b) == 0 {
		b = []byte{'\n'}
	}

	// How unsafe is this really?  Will this break if the GC becomes compacting?
	// Probably not, otherwise that would likely break &parse below as well.
	input := (*C.uchar)(unsafe.Pointer(&b[0]))
	C.yaml_parser_set_input_string(&p.parser, input, (C.size_t)(len(b)))

	p.skip()
	if p.event._type != C.YAML_STREAM_START_EVENT {
		panic("Expected stream start event, got " +
			strconv.Itoa(int(p.event._type)))
	}
	p.skip()
	return &p
}

func (p *parser) destroy() {
	if p.event._type != C.YAML_NO_EVENT {
		C.yaml_event_delete(&p.event)
	}
	C.yaml_parser_delete(&p.parser)
}

func (p *parser) skip() {
	if p.event._type != C.YAML_NO_EVENT {
		if p.event._type == C.YAML_STREAM_END_EVENT {
			panic("Attempted to go past the end of stream. Corrupted value?")
		}
		C.yaml_event_delete(&p.event)
	}
	if C.yaml_parser_parse(&p.parser, &p.event) == 0 {
		p.fail()
	}
}

func (p *parser) fail() {
	var where string
	var line int
	if p.parser.problem_mark.line != 0 {
		line = int(C.int(p.parser.problem_mark.line))
	} else if p.parser.context_mark.line != 0 {
		line = int(C.int(p.parser.context_mark.line))
	}
	if line != 0 {
		where = "line " + strconv.Itoa(line) + ": "
	}
	var msg string
	if p.parser.problem != nil {
		msg = C.GoString(p.parser.problem)
	} else {
		msg = "Unknown problem parsing YAML content"
	}
	panic(where + msg)
}

func (p *parser) anchor(n *node, anchor *C.yaml_char_t) {
	if anchor != nil {
		p.doc.anchors[stry(anchor)] = n
	}
}

func (p *parser) parse() *node {
	switch p.event._type {
	case C.YAML_SCALAR_EVENT:
		return p.scalar()
	case C.YAML_ALIAS_EVENT:
		return p.alias()
	case C.YAML_MAPPING_START_EVENT:
		return p.mapping()
	case C.YAML_SEQUENCE_START_EVENT:
		return p.sequence()
	case C.YAML_DOCUMENT_START_EVENT:
		return p.document()
	case C.YAML_STREAM_END_EVENT:
		// Happens when attempting to decode an empty buffer.
		return nil
	default:
		panic("Attempted to parse unknown event: " +
			strconv.Itoa(int(p.event._type)))
	}
	panic("Unreachable")
}

func (p *parser) node(kind int) *node {
	return &node{kind: kind,
		line:   int(C.int(p.event.start_mark.line)),
		column: int(C.int(p.event.start_mark.column))}
}

func (p *parser) document() *node {
	n := p.node(documentNode)
	n.anchors = make(map[string]*node)
	p.doc = n
	p.skip()
	n.children = append(n.children, p.parse())
	if p.event._type != C.YAML_DOCUMENT_END_EVENT {
		panic("Expected end of document event but got " +
			strconv.Itoa(int(p.event._type)))
	}
	p.skip()
	return n
}

func (p *parser) alias() *node {
	alias := C.event_alias(&p.event)
	n := p.node(aliasNode)
	n.value = stry(alias.anchor)
	p.skip()
	return n
}

func (p *parser) scalar() *node {
	scalar := C.event_scalar(&p.event)
	n := p.node(scalarNode)
	n.value = stry(scalar.value)
	n.tag = stry(scalar.tag)
	n.implicit = (scalar.plain_implicit != 0)
	p.anchor(n, scalar.anchor)
	p.skip()
	return n
}

func (p *parser) sequence() *node {
	n := p.node(sequenceNode)
	p.anchor(n, C.event_sequence_start(&p.event).anchor)
	p.skip()
	for p.event._type != C.YAML_SEQUENCE_END_EVENT {
		n.children = append(n.children, p.parse())
	}
	p.skip()
	return n
}

func (p *parser) mapping() *node {
	n := p.node(mappingNode)
	p.anchor(n, C.event_mapping_start(&p.event).anchor)
	p.skip()
	for p.event._type != C.YAML_MAPPING_END_EVENT {
		n.children = append(n.children, p.parse(), p.parse())
	}
	p.skip()
	return n
}

// ----------------------------------------------------------------------------
// Decoder, unmarshals a node into a provided value.

type decoder struct {
	doc     *node
	aliases map[string]bool
}

func newDecoder() *decoder {
	d := &decoder{}
	d.aliases = make(map[string]bool)
	return d
}

// d.setter deals with setters and pointer dereferencing and initialization.
//
// It's a slightly convoluted case to handle properly:
//
// - nil pointers should be initialized, unless being set to nil
// - we don't know at this point yet what's the value to SetYAML() with.
// - we can't separate pointer deref/init and setter checking, because
//   a setter may be found while going down a pointer chain.
//
// Thus, here is how it takes care of it:
//
// - out is provided as a pointer, so that it can be replaced.
// - when looking at a non-setter ptr, *out=ptr.Elem(), unless tag=!!null
// - when a setter is found, *out=interface{}, and a set() function is
//   returned to call SetYAML() with the value of *out once it's defined.
//
func (d *decoder) setter(tag string, out *reflect.Value, good *bool) (set func()) {
	again := true
	for again {
		again = false
		setter, _ := (*out).Interface().(Setter)
		if tag != "!!null" || setter != nil {
			if pv := (*out); pv.Kind() == reflect.Ptr {
				if pv.IsNil() {
					*out = reflect.New(pv.Type().Elem()).Elem()
					pv.Set((*out).Addr())
				} else {
					*out = pv.Elem()
				}
				setter, _ = pv.Interface().(Setter)
				again = true
			}
		}
		if setter != nil {
			var arg interface{}
			*out = reflect.ValueOf(&arg).Elem()
			return func() {
				*good = setter.SetYAML(tag, arg)
			}
		}
	}
	return nil
}

func (d *decoder) unmarshal(n *node, out reflect.Value) (good bool) {
	switch n.kind {
	case documentNode:
		good = d.document(n, out)
	case scalarNode:
		good = d.scalar(n, out)
	case aliasNode:
		good = d.alias(n, out)
	case mappingNode:
		good = d.mapping(n, out)
	case sequenceNode:
		good = d.sequence(n, out)
	default:
		panic("Internal error: unknown node kind: " + strconv.Itoa(n.kind))
	}
	return
}

func (d *decoder) document(n *node, out reflect.Value) (good bool) {
	if len(n.children) == 1 {
		d.doc = n
		d.unmarshal(n.children[0], out)
		return true
	}
	return false
}

func (d *decoder) alias(n *node, out reflect.Value) (good bool) {
	an, ok := d.doc.anchors[n.value]
	if !ok {
		panic("Unknown anchor '" + n.value + "' referenced")
	}
	if d.aliases[n.value] {
		panic("Anchor '" + n.value + "' value contains itself")
	}
	d.aliases[n.value] = true
	good = d.unmarshal(an, out)
	delete(d.aliases, n.value)
	return good
}

func (d *decoder) scalar(n *node, out reflect.Value) (good bool) {
	var tag string
	var resolved interface{}
	if n.tag == "" && !n.implicit {
		resolved = n.value
	} else {
		tag, resolved = resolve(n.tag, n.value)
		if set := d.setter(tag, &out, &good); set != nil {
			defer set()
		}
	}
	switch out.Kind() {
	case reflect.String:
		out.SetString(n.value)
		good = true
	case reflect.Interface:
		if resolved == nil {
			out.Set(reflect.Zero(out.Type()))
		} else {
			out.Set(reflect.ValueOf(resolved))
		}
		good = true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch resolved := resolved.(type) {
		case int:
			if !out.OverflowInt(int64(resolved)) {
				out.SetInt(int64(resolved))
				good = true
			}
		case int64:
			if !out.OverflowInt(resolved) {
				out.SetInt(resolved)
				good = true
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		switch resolved := resolved.(type) {
		case int:
			if resolved >= 0 {
				out.SetUint(uint64(resolved))
				good = true
			}
		case int64:
			if resolved >= 0 {
				out.SetUint(uint64(resolved))
				good = true
			}
		}
	case reflect.Bool:
		switch resolved := resolved.(type) {
		case bool:
			out.SetBool(resolved)
			good = true
		}
	case reflect.Float32, reflect.Float64:
		switch resolved := resolved.(type) {
		case float64:
			out.SetFloat(resolved)
			good = true
		}
	case reflect.Ptr:
		switch resolved.(type) {
		case nil:
			out.Set(reflect.Zero(out.Type()))
			good = true
		}
	}
	return good
}

func settableValueOf(i interface{}) reflect.Value {
	v := reflect.ValueOf(i)
	sv := reflect.New(v.Type()).Elem()
	sv.Set(v)
	return sv
}

func (d *decoder) sequence(n *node, out reflect.Value) (good bool) {
	if set := d.setter("!!seq", &out, &good); set != nil {
		defer set()
	}
	if out.Kind() == reflect.Interface {
		// No type hints. Will have to use a generic sequence.
		iface := out
		out = settableValueOf(make([]interface{}, 0))
		iface.Set(out)
	}

	if out.Kind() != reflect.Slice {
		return false
	}
	et := out.Type().Elem()

	l := len(n.children)
	for i := 0; i < l; i++ {
		e := reflect.New(et).Elem()
		if ok := d.unmarshal(n.children[i], e); ok {
			out.Set(reflect.Append(out, e))
		}
	}
	return true
}

func (d *decoder) mapping(n *node, out reflect.Value) (good bool) {
	if set := d.setter("!!map", &out, &good); set != nil {
		defer set()
	}
	if out.Kind() == reflect.Struct {
		return d.mappingStruct(n, out)
	}

	if out.Kind() == reflect.Interface {
		// No type hints. Will have to use a generic map.
		iface := out
		out = settableValueOf(make(map[interface{}]interface{}))
		iface.Set(out)
	}

	if out.Kind() != reflect.Map {
		return false
	}
	outt := out.Type()
	kt := outt.Key()
	et := outt.Elem()

	if out.IsNil() {
		out.Set(reflect.MakeMap(outt))
	}
	l := len(n.children)
	for i := 0; i < l; i += 2 {
		k := reflect.New(kt).Elem()
		if d.unmarshal(n.children[i], k) {
			e := reflect.New(et).Elem()
			if d.unmarshal(n.children[i+1], e) {
				out.SetMapIndex(k, e)
			}
		}
	}
	return true
}

func (d *decoder) mappingStruct(n *node, out reflect.Value) (good bool) {
	fields, err := getStructFields(out.Type())
	if err != nil {
		panic(err)
	}
	name := settableValueOf("")
	fieldsMap := fields.Map
	l := len(n.children)
	for i := 0; i < l; i += 2 {
		if !d.unmarshal(n.children[i], name) {
			continue
		}
		if info, ok := fieldsMap[name.String()]; ok {
			d.unmarshal(n.children[i+1], out.Field(info.Num))
		}
	}
	return true
}
