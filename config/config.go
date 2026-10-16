/**
 * @Author: LiuShuXin
 * @Description: 严格加载调用方配置，统一默认值、YAML 与环境变量的覆盖关系
 * @File: config.go
 * Software: Goland
 * @Date: 2026/10/09 18:02
 */

// Package config 提供严格配置加载机制，应用自行提供结构、默认值与校验。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// Options 由应用提供环境变量前缀和按点号分隔的字段默认值。
// EnvPrefix 为空时不读取环境变量；默认值必须使用目标结构中的叶子字段键名。
type Options struct {
	EnvPrefix string
	Defaults  map[string]any
	// DefaultsFrom 是与目标同类型的结构体或非 nil 结构体指针。
	// 可直接组合组件 DefaultOptions；Defaults 的显式键优先于这里的值。
	DefaultsFrom any
}

// Load 解码到非 nil 的结构体指针，优先级为环境变量 > YAML > 默认值。
// 成功前不修改目标，不执行应用语义校验，也不隐式发现配置文件。
func Load(path string, target any, options Options) error {
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return errors.New("config target must be a non-nil struct pointer")
	}
	keys, err := fieldKeys(value.Elem().Type(), "")
	if err != nil {
		return err
	}
	if options.EnvPrefix != "" {
		for i, ch := range options.EnvPrefix {
			if !(ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || i > 0 && ch >= '0' && ch <= '9') {
				return errors.New("config environment prefix must be an identifier")
			}
		}
	}
	v := viper.New()
	if options.DefaultsFrom != nil {
		defaults := reflect.ValueOf(options.DefaultsFrom)
		if defaults.Kind() == reflect.Pointer && !defaults.IsNil() {
			defaults = defaults.Elem()
		}
		if defaults.Type() != value.Elem().Type() {
			return errors.New("config defaults must have the same struct type as target")
		}
		defaults, err = cloneDefaultValue(defaults, 0)
		if err != nil {
			return err
		}
		setStructDefaults(v, defaults, "")
	}
	for key, defaultValue := range options.Defaults {
		if _, ok := keys[key]; !ok {
			return fmt.Errorf("unknown default configuration key %q", key)
		}
		copied, err := cloneDefaultValue(reflect.ValueOf(defaultValue), 0)
		if err != nil {
			return fmt.Errorf("invalid default configuration key %q: %w", key, err)
		}
		if copied.IsValid() {
			v.SetDefault(key, copied.Interface())
		} else {
			v.SetDefault(key, nil)
		}
	}
	if path != "" {
		settings, err := readYAML(path, value.Elem().Type())
		if err != nil {
			return err
		}
		if err := v.MergeConfigMap(settings); err != nil {
			return fmt.Errorf("merge configuration: %w", err)
		}
	}
	// 显式空值保留给应用校验，不能被默认值掩盖。
	v.AllowEmptyEnv(true)
	if options.EnvPrefix != "" {
		for key := range keys {
			name := options.EnvPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
			if err := v.BindEnv(key, name); err != nil {
				return fmt.Errorf("bind environment variable: %w", err)
			}
		}
	}
	decoded := reflect.New(value.Elem().Type())
	if err := v.UnmarshalExact(decoded.Interface(), environmentDecoder); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	value.Elem().Set(decoded.Elem())
	return nil
}

// fieldKeys 已校验结构和标签；在同一规则下提取叶子默认值，不引入第二套键名契约。
func setStructDefaults(v *viper.Viper, value reflect.Value, prefix string) {
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() || field.Tag.Get("mapstructure") == "-" {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("mapstructure"), ",")
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		key := prefix + name
		if field.Type.Kind() == reflect.Struct {
			setStructDefaults(v, value.Field(i), key+".")
		} else {
			v.SetDefault(key, value.Field(i).Interface())
		}
	}
}

// fieldKeys 按公开结构字段绑定环境变量，不依赖默认值是否覆盖全部字段。
func fieldKeys(schema reflect.Type, prefix string) (map[string]struct{}, error) {
	keys := make(map[string]struct{})
	for i := range schema.NumField() {
		field := schema.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("mapstructure")
		if tag == "-" {
			continue
		}
		name, flags, _ := strings.Cut(tag, ",")
		if flags != "" {
			return nil, fmt.Errorf("unsupported mapstructure options on %s", field.Name)
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if strings.ContainsAny(name, ". \t\r\n") {
			return nil, fmt.Errorf("invalid config field name %q", name)
		}
		key := prefix + name
		if field.Type.Kind() == reflect.Struct {
			nested, err := fieldKeys(field.Type, key+".")
			if err != nil {
				return nil, err
			}
			for nestedKey := range nested {
				if _, exists := keys[nestedKey]; exists {
					return nil, fmt.Errorf("duplicate config field %q", nestedKey)
				}
				keys[nestedKey] = struct{}{}
			}
		} else {
			if _, exists := keys[key]; exists {
				return nil, fmt.Errorf("duplicate config field %q", key)
			}
			keys[key] = struct{}{}
		}
	}
	return keys, nil
}

func environmentDecoder(options *mapstructure.DecoderConfig) {
	strictDecoder(options)
	// 原始 YAML 已严格校验，此处再转换环境变量中的标量和逗号列表。
	options.DecodeHook = func(from, to reflect.Type, value any) (any, error) {
		if from.Kind() == reflect.String && to.Kind() == reflect.Int {
			return strconv.Atoi(value.(string))
		}
		if from.Kind() == reflect.String && to.Kind() == reflect.Int64 && to != reflect.TypeFor[time.Duration]() {
			return strconv.ParseInt(value.(string), 10, 64)
		}
		if from.Kind() == reflect.String && to.Kind() == reflect.Bool {
			return strconv.ParseBool(value.(string))
		}
		if from.Kind() == reflect.String && to == reflect.TypeFor[[]string]() {
			if value == "" {
				return []string{}, nil
			}
			parts := strings.Split(value.(string), ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts, nil
		}
		return strictInteger(from, to, value)
	}
}

func readYAML(path string, schemaType reflect.Type) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var settings map[string]any
	if err := decoder.Decode(&settings); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if settings == nil {
		return nil, fmt.Errorf("config %q must contain a YAML mapping", path)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config %q must contain exactly one YAML document", path)
	}

	// 先校验原始映射：Viper 展平可能丢弃空的未知字段，
	// 环境变量覆盖也可能掩盖文件中的类型错误。
	if err := rejectNull(settings, ""); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	schema := reflect.New(schemaType)
	options := &mapstructure.DecoderConfig{Result: schema.Interface(), ErrorUnused: true}
	strictDecoder(options)
	structureDecoder, err := mapstructure.NewDecoder(options)
	if err != nil {
		return nil, fmt.Errorf("create config decoder: %w", err)
	}
	if err := structureDecoder.Decode(settings); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return settings, nil
}

func strictDecoder(options *mapstructure.DecoderConfig) {
	options.WeaklyTypedInput = false
	options.MatchName = func(key, field string) bool { return key == field }
	options.DecodeHook = strictInteger
}

func strictInteger(from, to reflect.Type, value any) (any, error) {
	if to == reflect.TypeFor[time.Duration]() {
		if from == to {
			return value, nil
		}
		if from.Kind() != reflect.String {
			return nil, errors.New("duration must be a string with a unit, e.g. 5s")
		}
		return time.ParseDuration(value.(string))
	}
	// mapstructure 即使关闭弱类型转换，也会把浮点数截断为整数。
	if to.Kind() == reflect.Int && from.Kind() != reflect.Int {
		return nil, fmt.Errorf("expected integer, got %s", from)
	}
	if to.Kind() == reflect.Int64 && from.Kind() != reflect.Int && from.Kind() != reflect.Int64 {
		return nil, fmt.Errorf("expected integer, got %s", from)
	}
	return value, nil
}

func rejectNull(settings map[string]any, prefix string) error {
	for key, value := range settings {
		path := prefix + key
		if value == nil {
			return fmt.Errorf("%s must not be null; omit the key to use its default", path)
		}
		if err := rejectNullValue(value, path); err != nil {
			return err
		}
	}
	return nil
}

func rejectNullValue(value any, path string) error {
	if value == nil {
		return fmt.Errorf("%s must not be null; omit the key to use its default", path)
	}
	switch nested := value.(type) {
	case map[string]any:
		return rejectNull(nested, path+".")
	case []any:
		for i, item := range nested {
			if err := rejectNullValue(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
