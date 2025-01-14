package yarql

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"mime/multipart"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// AttrIsID can be added to a method response to make it a ID field
// For example:
// func (Foo) ResolveExampleMethod() (string, AttrIsID) {
//   return "i'm an ID type now", 0
// }
//
// Not that the response value doesn't matter
type AttrIsID uint8

type types map[string]*obj

func (t *types) Add(obj obj) obj {
	if obj.valueType != valueTypeObj && obj.valueType != valueTypeInterface {
		panic("Can only add struct types to list")
	}

	val := *t
	val[obj.typeName] = &obj
	*t = val

	return obj.getRef()
}

func (t *types) Get(key string) (*obj, bool) {
	val, ok := (*t)[key]
	return val, ok
}

// Schema defines the graphql schema
type Schema struct {
	parsed bool

	types      types
	inTypes    inputMap
	interfaces types

	rootQuery         *obj
	rootQueryValue    reflect.Value
	rootMethod        *obj
	rootMethodValue   reflect.Value
	MaxDepth          uint8 // Default 255
	definedEnums      []enum
	definedDirectives map[DirectiveLocation][]*Directive
	ctx               *Ctx

	// Zero alloc variables
	Result           []byte
	graphqlTypesMap  map[string]qlType
	graphqlTypesList []qlType
	graphqlObjFields map[string][]qlField
}

type valueType int

const (
	valueTypeUndefined valueType = iota
	valueTypeArray
	valueTypeObjRef
	valueTypeObj
	valueTypeData
	valueTypePtr
	valueTypeMethod
	valueTypeEnum
	valueTypeTime
	valueTypeInterfaceRef
	valueTypeInterface
)

// TODO Maybe add a pointer to the opj if valueType == valueTypeObjRef || valueType == valueTypeInterfaceRef
//   Now we have to do a map lookup and that's quite slow

type obj struct {
	valueType     valueType
	typeName      string
	typeNameBytes []byte
	goTypeName    string
	goPkgPath     string
	qlFieldName   []byte
	hidden        bool
	isID          bool

	// Value type == valueTypeObj || valueTypeInterface
	objContents map[uint32]*obj

	// Value type == valueTypeObj
	customObjValue *reflect.Value // Mainly Graphql internal values like __schema

	// Value is inside struct
	structFieldIdx int

	// Value type == valueTypeArray || type == valueTypePtr
	innerContent *obj

	// Value type == valueTypeData
	dataValueType reflect.Kind

	// Value type == valueTypeMethod
	method *objMethod

	// Value type == valueTypeEnum
	enumTypeIndex int

	// Value type == valueTypeInterface || valueTypeObj
	implementations []*obj
}

func getObjKey(key []byte) uint32 {
	hasher := fnv.New32()
	hasher.Write(key)
	return hasher.Sum32()
}

func (o *obj) getRef() obj {
	switch o.valueType {
	case valueTypeObj:
		return obj{
			valueType:     valueTypeObjRef,
			typeName:      o.typeName,
			goTypeName:    o.goTypeName,
			goPkgPath:     o.goPkgPath,
			typeNameBytes: []byte(o.typeName),
		}
	case valueTypeInterface:
		return obj{
			valueType:     valueTypeInterfaceRef,
			typeName:      o.typeName,
			goTypeName:    o.goTypeName,
			goPkgPath:     o.goPkgPath,
			typeNameBytes: []byte(o.typeName),
		}
	default:
		panic("getRef can only be used on objects")
	}
}

type objMethod struct {
	// Is this a function field inside this object or a method attached to the struct
	// true = func (*someStruct) ResolveFooBar() string {}
	// false = ResolveFooBar func() string
	isTypeMethod   bool
	goFunctionName string
	goType         reflect.Type

	ins        []baseInput             // The real function inputs
	inFields   map[string]referToInput // Contains all the fields of all the ins
	checkedIns bool                    // are the ins checked yet

	outNr      int
	outType    obj
	errorOutNr *int
}

type inputMap map[string]*input

type referToInput struct {
	inputIdx int // the method's argument index
	input    input
}

type input struct {
	kind reflect.Kind

	// Is this a custom type?
	isEnum        bool
	enumTypeIndex int
	isID          bool
	isFile        bool
	isTime        bool

	goFieldIdx  int
	gqFieldName string

	// kind == Slice, Array or Ptr
	elem *input

	// kind == struct
	isStructPointers bool
	structName       string
	structContent    map[string]input
}

type baseInput struct {
	isCtx  bool
	goType *reflect.Type
}

// SchemaOptions are options for creating a new schema
type SchemaOptions struct {
	// only used for for testing
	noMethodEqualToQueryChecks bool

	SkipGraphqlTypesInjection bool
}

type parseCtx struct {
	schema             *Schema
	unknownTypesCount  int
	unknownInputsCount int
	parsedMethods      []*objMethod
}

// NewSchema creates a new schema wherevia you can define the graphql types and make queries
func NewSchema() *Schema {
	s := &Schema{
		types:             types{},
		inTypes:           inputMap{},
		interfaces:        types{},
		MaxDepth:          255,
		graphqlObjFields:  map[string][]qlField{},
		definedEnums:      []enum{},
		definedDirectives: map[DirectiveLocation][]*Directive{},
		Result:            make([]byte, 16384),
	}

	added, err := s.RegisterEnum(directiveLocationMap)
	if err != nil {
		panic("INTERNAL ERROR: " + err.Error())
	}
	if !added {
		panic("INTERNAL ERROR: directive locations ENUM should be added")
	}

	s.RegisterEnum(typeKindEnumMap)
	if err != nil {
		panic("INTERNAL ERROR: " + err.Error())
	}
	if !added {
		panic("INTERNAL ERROR: type kind ENUM should be added")
	}

	err = s.RegisterDirective(Directive{
		Name: "skip",
		Where: []DirectiveLocation{
			DirectiveLocationField,
			DirectiveLocationFragment,
			DirectiveLocationFragmentInline,
		},
		Method: func(args struct{ If bool }) DirectiveModifier {
			return DirectiveModifier{
				Skip: args.If,
			}
		},
		Description: "Directs the executor to skip this field or fragment when the `if` argument is true.",
	})
	if err != nil {
		panic("INTERNAL ERROR: " + err.Error())
	}

	err = s.RegisterDirective(Directive{
		Name: "include",
		Where: []DirectiveLocation{
			DirectiveLocationField,
			DirectiveLocationFragment,
			DirectiveLocationFragmentInline,
		},
		Method: func(args struct{ If bool }) DirectiveModifier {
			return DirectiveModifier{
				Skip: !args.If,
			}
		},
		Description: "Directs the executor to include this field or fragment only when the `if` argument is true.",
	})
	if err != nil {
		panic("INTERNAL ERROR: " + err.Error())
	}

	return s
}

// SetCacheRules sets the cacheing rules
func (s *Schema) SetCacheRules(
	cacheQueryFromLen *int, // default = 300
) {
	if cacheQueryFromLen != nil {
		s.ctx.query.CacheableQueryMinLen = *cacheQueryFromLen
	}
}

// Parse parses your queries and methods
func (s *Schema) Parse(queries interface{}, methods interface{}, options *SchemaOptions) error {
	s.rootQueryValue = reflect.ValueOf(queries)
	s.rootMethodValue = reflect.ValueOf(methods)

	ctx := &parseCtx{
		schema:        s,
		parsedMethods: []*objMethod{},
	}

	obj, err := ctx.check(reflect.TypeOf(queries), false)
	if err != nil {
		return err
	}
	if obj.valueType != valueTypeObjRef {
		return errors.New("input queries must be a struct")
	}
	s.rootQuery = s.types[obj.typeName]

	obj, err = ctx.check(reflect.TypeOf(methods), false)
	if err != nil {
		return err
	}
	if obj.valueType != valueTypeObjRef {
		return errors.New("input methods must be a struct")
	}
	s.rootMethod = s.types[obj.typeName]

	if options == nil || !options.noMethodEqualToQueryChecks {
		queryPkg := s.rootQuery.goPkgPath + s.rootQuery.goTypeName
		methodPkg := s.rootMethod.goPkgPath + s.rootMethod.goTypeName
		if queryPkg == methodPkg {
			return errors.New("method and query cannot be the same struct")
		}
	}

	if options == nil || !options.SkipGraphqlTypesInjection {
		s.injectQLTypes(ctx)
	}

	for _, method := range ctx.parsedMethods {
		err = ctx.checkFunctionIns(method)
		if err != nil {
			return err
		}
	}

	for _, directiveLocation := range ctx.schema.definedDirectives {
		for _, directive := range directiveLocation {
			if directive.parsedMethod.checkedIns {
				continue
			}

			err = ctx.checkFunctionIns(directive.parsedMethod)
			if err != nil {
				return err
			}
		}
	}

	s.ctx = newCtx(s)
	s.parsed = true

	return nil
}

func (c *parseCtx) check(t reflect.Type, hasIDTag bool) (*obj, error) {
	res := obj{
		typeNameBytes: []byte(t.Name()),
		typeName:      t.Name(),
		goPkgPath:     t.PkgPath(),
		goTypeName:    t.Name(),
	}

	if res.goPkgPath == "time" && res.goTypeName == "Time" {
		res.valueType = valueTypeTime
		return &res, nil
	}

	switch t.Kind() {
	case reflect.Struct:
		if hasIDTag {
			return nil, errors.New("structs cannot have ID attribute")
		}
		if res.typeName != "" {
			newName, ok := renamedTypes[res.typeName]
			if ok {
				res.typeName = newName
				res.typeNameBytes = []byte(newName)
			}

			v, ok := c.schema.types.Get(res.typeName)
			if ok {
				if v.goPkgPath != res.goPkgPath {
					return nil, fmt.Errorf("cannot have 2 structs with same type name: %s(%s) != %s(%s)", v.goPkgPath, res.goTypeName, res.goPkgPath, res.goTypeName)
				}

				res = v.getRef()
				return &res, nil
			}

			implementations := structImplementsMap[t.Name()]
			for _, implementation := range implementations {
				impl, err := c.check(implementation, false)
				if err != nil {
					return nil, err
				}
				res.implementations = append(res.implementations, impl)
			}
		} else {
			c.unknownTypesCount++
			res.typeName = "__UnknownType" + strconv.Itoa(c.unknownTypesCount)
			res.typeNameBytes = []byte(res.typeName)
		}

		res.valueType = valueTypeObj
		res.objContents = map[uint32]*obj{}

		typesInner := c.schema.types
		typesInner[res.typeName] = &res
		c.schema.types = typesInner
		c.checkStructFieldRecursive(t, &res)
	case reflect.Array, reflect.Slice, reflect.Ptr:
		isPtr := t.Kind() == reflect.Ptr
		if isPtr {
			res.valueType = valueTypePtr
		} else {
			res.valueType = valueTypeArray
		}

		obj, err := c.check(t.Elem(), hasIDTag && isPtr)
		if err != nil {
			return nil, err
		}
		res.innerContent = obj
	case reflect.Interface:
		if hasIDTag {
			return nil, errors.New("interface cannot have ID attribute")
		}
		if res.typeName == "" {
			return nil, errors.New("inline interfaces not allowed")
		}

		newName, ok := renamedTypes[res.typeName]
		if ok {
			res.typeName = newName
			res.typeNameBytes = []byte(newName)
		}

		v, ok := c.schema.interfaces.Get(res.typeName)
		if ok {
			if v.goPkgPath != res.goPkgPath {
				return nil, fmt.Errorf("cannot have 2 interfaces with same type name: %s(%s) != %s(%s)", v.goPkgPath, res.goTypeName, res.goPkgPath, res.goTypeName)
			}

			res = v.getRef()
			return &res, nil
		}

		res.valueType = valueTypeInterface
		res.implementations = []*obj{}
		res.objContents = map[uint32]*obj{}

		// Store the interface so we don't get an infinite loop and can reference this one
		interfaces := c.schema.interfaces
		interfaces[res.typeName] = &res
		c.schema.interfaces = interfaces

		methodPkgName := res.goPkgPath + "." + res.goTypeName
		if res.goTypeName == "" {
			methodPkgName = "inline interface"
		}

		typesThatImplementInterface, ok := implementationMap[t.Name()]
		if !ok {
			return nil, errors.New("cannot register a interface without explicit implementations")
		}
		for _, interfaceType := range typesThatImplementInterface {
			if interfaceType.Kind() != reflect.Struct {
				return nil, fmt.Errorf("only struct types are allowed as (%s).Is(types...) arguments", methodPkgName)
			}
			if interfaceType.Name() == "" {
				return nil, fmt.Errorf("inline struct not allowed in (%s).Is(types...)", methodPkgName)
			}
			if !interfaceType.Implements(t) {
				return nil, fmt.Errorf("(%s).Is(types...): %s.%s does not implement %s", methodPkgName, interfaceType.PkgPath(), interfaceType.Name(), methodPkgName)
			}

			obj, err := c.check(interfaceType, false)
			if err != nil {
				return nil, err
			}

			res.implementations = append(res.implementations, obj)
		}
	case reflect.Func, reflect.Map, reflect.Chan, reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		return nil, fmt.Errorf("unsupported value type %s", t.Kind().String())
	default:
		enumIndex, enum := c.schema.getEnum(t)
		if enum != nil {
			res.valueType = valueTypeEnum
			res.enumTypeIndex = enumIndex
		} else {
			res.valueType = valueTypeData
			res.dataValueType = t.Kind()

			if hasIDTag {
				res.isID = hasIDTag
				err := checkValidIDKind(res.dataValueType)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	if res.valueType == valueTypeObj || res.valueType == valueTypeInterface {
		for i := 0; i < t.NumMethod(); i++ {
			method := t.Method(i)
			methodObj, name, isID, err := c.checkFunction(method.Name, method.Type, true, false)
			if err != nil {
				return nil, err
			} else if methodObj == nil {
				continue
			}

			qlFieldName := []byte(name)
			res.objContents[getObjKey(qlFieldName)] = &obj{
				qlFieldName:    qlFieldName,
				valueType:      valueTypeMethod,
				goPkgPath:      method.PkgPath,
				goTypeName:     method.Name,
				structFieldIdx: i,
				method:         methodObj,
				isID:           isID,
			}
		}

		if res.valueType == valueTypeInterface {
			res = c.schema.interfaces.Add(res)
		} else {
			res = c.schema.types.Add(res)
		}
		// res is now a objPtr pointing to an obj or a interfacePtr pointing to an interface
	}

	return &res, nil
}

func (c *parseCtx) checkStructFieldRecursive(t reflect.Type, res *obj) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous {
			c.checkStructFieldRecursive(field.Type, res)
		}

		customName, obj, err := c.checkStructField(field, i)
		if err != nil {
			return
		}
		if obj != nil {
			name := formatGoNameToQL(field.Name)
			if customName != nil {
				name = *customName
			}
			obj.qlFieldName = []byte(name)

			res.objContents[getObjKey(obj.qlFieldName)] = obj
		}
	}
}

func (c *parseCtx) checkStructField(field reflect.StructField, idx int) (customName *string, obj *obj, err error) {
	if field.Anonymous {
		return nil, nil, nil
	}

	var ignore, isID bool
	customName, ignore, isID, err = parseFieldTagGQ(&field)
	if ignore || err != nil {
		return nil, nil, err
	}

	if field.Type.Kind() == reflect.Func {
		obj, err = c.checkStructFieldFunc(field.Name, field.Type, isID, idx)
	} else {
		obj, err = c.check(field.Type, isID)
	}

	if obj != nil {
		obj.structFieldIdx = idx
	}
	return
}

func (c *parseCtx) checkStructFieldFunc(fieldName string, goType reflect.Type, hasIDTag bool, idx int) (*obj, error) {
	methodObj, _, isID, err := c.checkFunction(fieldName, goType, false, hasIDTag)
	if err != nil {
		return nil, err
	}
	if methodObj == nil {
		return nil, nil
	}
	return &obj{
		valueType:      valueTypeMethod,
		method:         methodObj,
		structFieldIdx: idx,
		isID:           isID,
	}, nil
}

var ctxType = reflect.TypeOf(Ctx{})

func isCtx(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && ctxType.Name() == t.Name() && ctxType.PkgPath() == t.PkgPath()
}

func (c *parseCtx) checkFunctionInputStruct(field *reflect.StructField, idx int) (res input, skipThisField bool, err error) {
	wrapErr := func(err error) error {
		return fmt.Errorf("%s, struct field: %s", err.Error(), field.Name)
	}

	if field.Anonymous {
		// skip field
		return res, true, nil
	}

	newName, ignore, isID, err := parseFieldTagGQ(field)
	if ignore {
		// skip field
		return res, true, nil
	}
	if err != nil {
		return res, false, wrapErr(err)
	}

	qlFieldName := formatGoNameToQL(field.Name)
	if newName != nil {
		qlFieldName = *newName
	}

	res, err = c.checkFunctionInput(field.Type, isID)
	if err != nil {
		return input{}, false, wrapErr(err)
	}

	res.goFieldIdx = idx
	res.gqFieldName = qlFieldName

	return
}

func (c *parseCtx) checkFunctionInput(t reflect.Type, hasIDTag bool) (input, error) {
	kind := t.Kind()
	res := input{
		kind: kind,
	}

	switch kind {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		enumIndex, enum := c.schema.getEnum(t)
		if enum != nil {
			res.isEnum = true
			res.enumTypeIndex = enumIndex
		} else if hasIDTag {
			res.isID = true
			err := checkValidIDKind(res.kind)
			if err != nil {
				return res, err
			}
		}
	case reflect.Ptr:
		if t.AssignableTo(reflect.TypeOf(&multipart.FileHeader{})) {
			// This is a file header, these are handled completely different from a normal pointer
			res.isFile = true
			return res, nil
		}

		input, err := c.checkFunctionInput(t.Elem(), hasIDTag)
		if err != nil {
			return res, err
		}
		res.elem = &input
	case reflect.Array, reflect.Slice:
		input, err := c.checkFunctionInput(t.Elem(), false)
		if err != nil {
			return res, err
		}
		res.elem = &input
	case reflect.Struct:
		if t.AssignableTo(reflect.TypeOf(time.Time{})) {
			// This is a time property, these are handled completely different from a normal struct
			return input{
				kind:   reflect.String,
				isTime: true,
			}, nil
		}

		structName := t.Name()
		if len(structName) == 0 {
			c.unknownInputsCount++
			structName = "__UnknownInput" + strconv.Itoa(c.unknownInputsCount)
		} else {
			newStructName, ok := renamedTypes[structName]
			if ok {
				structName = newStructName
			}
			_, equalTypeExist := c.schema.types[structName]
			if equalTypeExist {
				// types and inputs with the same name are not allowed in graphql, add __input as suffix
				// TODO allow this value to be filledin by the user
				structName = structName + "__input"
			}
		}

		_, ok := c.schema.inTypes[structName]
		if !ok {
			// Make sure the input types entry is set before looping over it's fields to fix the n+1 problem
			c.schema.inTypes[structName] = &res

			res.structName = structName
			res.structContent = map[string]input{}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				input, skip, err := c.checkFunctionInputStruct(&field, i)
				if skip {
					continue
				}
				if err != nil {
					return res, err
				}
				res.structContent[input.gqFieldName] = input
			}
		}

		return input{
			kind:             kind,
			structName:       structName,
			isStructPointers: true,
		}, nil
	case reflect.Map, reflect.Func:
		// TODO: maybe we can do something with these
		fallthrough
	default:
		return res, fmt.Errorf("unsupported type %s", kind.String())
	}

	return res, nil
}

func (c *parseCtx) checkFunction(name string, t reflect.Type, isTypeMethod bool, hasIDTag bool) (method *objMethod, qlName string, isID bool, err error) {
	isID = hasIDTag

	trimmedName := name

	if strings.HasPrefix(name, "Resolve") {
		if len(name) > 0 {
			trimmedName = strings.TrimPrefix(name, "Resolve")
			if isTypeMethod && strings.ToUpper(string(trimmedName[0]))[0] != trimmedName[0] {
				// Resolve name must start with a uppercase letter
				return
			}
		} else if isTypeMethod {
			return
		}
	} else if isTypeMethod {
		return
	}

	if t.IsVariadic() {
		err = errors.New("function method cannot end with spread operator (...string)")
		return
	}

	numOuts := t.NumOut()
	if numOuts == 0 {
		err = fmt.Errorf("%s no value returned", name)
		return
	}

	var outNr *int
	var outTypeObj *obj
	var hasErrorOut *int

	errInterface := reflect.TypeOf((*error)(nil)).Elem()
	attrIsIDType := reflect.TypeOf(AttrIsID(0))
	for i := 0; i < numOuts; i++ {
		outType := t.Out(i)
		outKind := outType.Kind()
		if outType.Name() == attrIsIDType.Name() && outType.PkgPath() == attrIsIDType.PkgPath() {
			isID = true
		} else if outKind == reflect.Interface && outType.Implements(errInterface) {
			if hasErrorOut != nil {
				err = fmt.Errorf("%s cannot return multiple error types", name)
				return
			}
			hasErrorOut = func(i int) *int {
				return &i
			}(i)
		} else {
			if outNr != nil {
				err = fmt.Errorf("%s cannot return multiple types of data", name)
				return
			}

			outNr = func(i int) *int {
				return &i
			}(i)
		}
	}

	if outNr == nil {
		err = fmt.Errorf("%s does not return usable data", name)
		return
	}

	outTypeObj, err = c.check(t.Out(*outNr), isID)
	if err != nil {
		return
	}

	res := &objMethod{
		goType:         t,
		goFunctionName: name,
		isTypeMethod:   isTypeMethod,
		ins:            []baseInput{},
		inFields:       map[string]referToInput{},
		outNr:          *outNr,
		outType:        *outTypeObj,
		errorOutNr:     hasErrorOut,
	}
	c.parsedMethods = append(c.parsedMethods, res)
	return res, formatGoNameToQL(trimmedName), isID, nil
}

func (c *parseCtx) checkFunctionIns(method *objMethod) error {
	totalInputs := method.goType.NumIn()
	for i := 0; i < totalInputs; i++ {
		iInList := i
		if method.isTypeMethod {
			if i == 0 {
				// First argument can be skipped if type method
				continue
			}
			iInList = i - 1
		}

		goType := method.goType.In(i)
		input := baseInput{}
		typeKind := goType.Kind()
		if typeKind == reflect.Ptr && isCtx(goType.Elem()) {
			input.isCtx = true
		} else if isCtx(goType) {
			return fmt.Errorf("%s ctx argument must be a pointer", method.goFunctionName)
		} else if typeKind == reflect.Struct {
			input.goType = &goType
			for i := 0; i < goType.NumField(); i++ {
				field := goType.Field(i)
				input, skip, err := c.checkFunctionInputStruct(&field, i)
				if skip {
					continue
				}
				if err != nil {
					return fmt.Errorf("%s, type %s (#%d)", err.Error(), goType.Name(), i)
				}

				method.inFields[input.gqFieldName] = referToInput{
					inputIdx: iInList,
					input:    input,
				}
			}
		} else {
			return fmt.Errorf("invalid struct item type %s (#%d)", goType.Name(), i)
		}

		method.ins = append(method.ins, input)
	}

	method.checkedIns = true
	return nil
}

func formatGoNameToQL(input string) string {
	if len(input) <= 1 {
		return strings.ToLower(input)
	}

	if input[1] == bytes.ToUpper([]byte{input[1]})[0] {
		// Don't change names like: INPUT to iNPUT
		return input
	}

	return string(bytes.ToLower([]byte{input[0]})) + input[1:]
}

func parseFieldTagGQ(field *reflect.StructField) (newName *string, ignore bool, isID bool, err error) {
	val, ok := field.Tag.Lookup("gq")
	if !ok {
		return
	}
	if val == "" {
		return
	}

	args := strings.Split(val, ",")
	nameArg := strings.TrimSpace(args[0])
	if nameArg != "" {
		if nameArg == "-" {
			ignore = true
			return
		}
		err = validGraphQlName([]byte(nameArg))
		newName = &nameArg
	}

	for _, modifier := range args[1:] {
		switch strings.ToLower(strings.TrimSpace(modifier)) {
		case "id":
			isID = true
		default:
			err = fmt.Errorf("unknown field tag gq argument: %s", modifier)
			return
		}
	}

	return
}

func validGraphQlName(name []byte) error {
	if len(name) == 0 {
		return errors.New("invalid graphql name")
	}
	for i, char := range name {
		if char >= 'A' && char <= 'Z' {
			continue
		}
		if char >= 'a' && char <= 'z' {
			continue
		}
		if i > 0 {
			if char >= '0' && char <= '9' {
				continue
			}
			if char == '_' {
				continue
			}
		}
		return errors.New("invalid graphql name")
	}
	return nil
}

func checkValidIDKind(kind reflect.Kind) error {
	switch kind {
	case reflect.String:
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
	default:
		return errors.New("strings and numbers can only be labeld with the ID property")
	}
	return nil
}

func (s *Schema) objToQlTypeName(item *obj, target *bytes.Buffer) {
	suffix := []byte{}

	qlType := wrapQLTypeInNonNull(s.objToQLType(item))

	for {
		switch qlType.Kind {
		case typeKindList:
			target.WriteByte('[')
			suffix = append(suffix, ']')
		case typeKindNonNull:
			suffix = append(suffix, '!')
		default:
			if qlType.Name != nil {
				target.WriteString(*qlType.Name)
			} else {
				target.Write([]byte("Unknown"))
			}
			if len(suffix) > 0 {
				target.Write(suffix)
			}
			return
		}
		qlType = qlType.OfType
	}
}
