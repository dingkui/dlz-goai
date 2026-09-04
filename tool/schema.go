package tool

import (
	"reflect"
	"sort"
	"strings"
)

// SchemaFor 从 Go 类型反射生成 JSON Schema（当前支持一个 Schema 草案子集，
// 覆盖最常见的参数形态）。struct 字段名优先取 json tag；非指针且
// 未声明 omitempty 的字段进入 required。
//
// 不支持的类型（chan、func 等）降级为空 schema（任意值），不报错——
// Schema 是给模型看的提示，不是安全边界，宽松比失败更有用。
func SchemaFor(sample any) map[string]any {
	t := reflect.TypeOf(sample)
	if t == nil {
		return objectSchema(nil, nil)
	}
	return schemaOfType(t)
}

func schemaOfType(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaOfType(t.Elem())}
	case reflect.Map:
		schema := map[string]any{"type": "object"}
		if t.Key().Kind() == reflect.String {
			schema["additionalProperties"] = schemaOfType(t.Elem())
		}
		return schema
	case reflect.Struct:
		return structSchema(t)
	default:
		// any、chan、func 等：不约束
		return map[string]any{}
	}
}

func structSchema(t reflect.Type) map[string]any {
	properties := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		name := field.Name
		omit := false
		if tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
			for _, option := range parts[1:] {
				if option == "omitempty" {
					omit = true
				}
			}
		}
		fieldType := field.Type
		isPointer := fieldType.Kind() == reflect.Ptr
		properties[name] = schemaOfType(fieldType)
		// 非指针且未声明 omitempty 视为必填；指针天然可空
		if !isPointer && !omit {
			required = append(required, name)
		}
	}
	sort.Strings(required) // 输出稳定，同样的结构体生成同样的 schema
	return objectSchema(properties, required)
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object"}
	if properties == nil {
		properties = map[string]any{}
	}
	schema["properties"] = properties
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
