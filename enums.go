package graphql

import (
	"fmt"
	"reflect"
	"sort"

	h "github.com/mjarkk/go-graphql/helpers"
)

type enum struct {
	contentType reflect.Type
	typeName    string
	keyValue    map[string]reflect.Value
	valueKey    reflect.Value
	qlType      qlType
}

func getEnum(t reflect.Type) *enum {
	if len(t.PkgPath()) == 0 || len(t.Name()) == 0 || !validEnumType(t) {
		return nil
	}

	enum, ok := definedEnums[t.Name()]
	if !ok {
		return nil
	}

	return &enum
}

func validEnumType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// All int kinds are allowed
		return true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// All uint kinds are allowed
		return true
	case reflect.String:
		// Strings are allowed
		return true
	default:
		return false
	}
}

var definedEnums = map[string]enum{}

func RegisterEnum(map_ interface{}) bool {
	enum := registerEnumCheck(map_)
	if enum == nil {
		return false
	}

	definedEnums[enum.typeName] = *enum
	return true
}

func registerEnumCheck(map_ interface{}) *enum {
	mapReflection := reflect.ValueOf(map_)
	mapType := mapReflection.Type()

	invalidTypeMsg := fmt.Sprintf("RegisterEnum input must be of type map[string]CustomType(int..|uint..|string) as input, %+v given", map_)

	if mapType.Kind() != reflect.Map {
		// Tye input type must be a map
		panic(invalidTypeMsg)
	}
	if mapType.Key().Kind() != reflect.String {
		// The map key must be a string
		panic(invalidTypeMsg)
	}
	contentType := mapType.Elem()
	if !validEnumType(contentType) {
		panic(invalidTypeMsg)
	}

	if contentType.PkgPath() == "" || contentType.Name() == "" {
		panic("RegisterEnum input map value must have a global custom type value (type Animals string) or (type Rules uint64)")
	}

	inputLen := mapReflection.Len()
	if inputLen == 0 {
		// No point in registering enums with 0 items
		return nil
	}

	res := map[string]reflect.Value{}
	valueKeyMap := reflect.MakeMapWithSize(reflect.MapOf(contentType, reflect.TypeOf("")), inputLen)

	iter := mapReflection.MapRange()
	for iter.Next() {
		k := iter.Key()
		keyStr := k.Interface().(string)
		if keyStr == "" {
			panic("RegisterEnum input map cannot contain empty keys")
		}

		err := validGraphQlName(keyStr)
		if err != nil {
			panic(fmt.Sprintf("RegisterEnum map key must start with an alphabetic character (lower or upper) followed by the same or a \"_\", key given: %s", keyStr))
		}

		v := reflect.ValueOf(iter.Value().Interface())
		if valueKeyMap.MapIndex(v).IsValid() {
			panic(fmt.Sprintf("RegisterEnum input map cannot have duplicated values, value: %+v", v.Interface()))
		}
		valueKeyMap.SetMapIndex(v, reflect.ValueOf(keyStr))
		res[keyStr] = v
	}

	name := contentType.Name()

	qlTypeEnumValues := []qlEnumValue{}
	for key := range res {
		qlTypeEnumValues = append(qlTypeEnumValues, qlEnumValue{
			Name:              key,
			Description:       h.StrPtr(""),
			IsDeprecated:      false,
			DeprecationReason: nil,
		})
	}
	sort.Slice(qlTypeEnumValues, func(a int, b int) bool { return qlTypeEnumValues[a].Name < qlTypeEnumValues[b].Name })

	qlType := qlType{
		Kind:        typeKindEnum,
		Name:        &name,
		Description: h.StrPtr(""),
		EnumValues: func(args isDeprecatedArgs) []qlEnumValue {
			return qlTypeEnumValues
		},
	}

	return &enum{
		contentType: contentType,
		keyValue:    res,
		valueKey:    valueKeyMap,
		typeName:    name,
		qlType:      qlType,
	}
}
