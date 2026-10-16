/**
 * @Author: LiuShuXin
 * @Description: 隔离默认配置中的可变引用，防止解码结果污染默认值或其他加载结果
 * @File: defaults.go
 * Software: Goland
 * @Date: 2026/10/16
 */
package config

import (
	"errors"
	"fmt"
	"reflect"
)

const maxDefaultDepth = 64

// 解码器遇到相同类型的结构体会直接赋值，不能靠重新分配顶层目标隔离
// []struct/map[string]struct 内部的引用。先复制数据，再交给 Viper 和解码器。
// 配置是有限的数据树，限制嵌套层数也让循环指针/map 返回错误而不是栈溢出。
func cloneDefaultValue(value reflect.Value, depth int) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}
	if depth > maxDefaultDepth {
		return reflect.Value{}, errors.New("config defaults exceed supported nesting depth or contain a cycle")
	}
	result := reflect.New(value.Type()).Elem()
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return result, nil
		}
		element, err := cloneDefaultValue(value.Elem(), depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		if value.Kind() == reflect.Pointer {
			result.Set(reflect.New(value.Type().Elem()))
			result.Elem().Set(element)
		} else {
			result.Set(element)
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice {
			if value.IsNil() {
				return result, nil
			}
			result.Set(reflect.MakeSlice(value.Type(), value.Len(), value.Len()))
		}
		for i := 0; i < value.Len(); i++ {
			element, err := cloneDefaultValue(value.Index(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(element)
		}
	case reflect.Map:
		if value.IsNil() {
			return result, nil
		}
		result.Set(reflect.MakeMapWithSize(value.Type(), value.Len()))
		iterator := value.MapRange()
		for iterator.Next() {
			key, err := cloneDefaultValue(iterator.Key(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			element, err := cloneDefaultValue(iterator.Value(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(key, element)
		}
	case reflect.Struct:
		// 保留值类型的内部表示。被跳过字段只能浅拷贝，因此先确认它们
		// 不携带可变引用；不能静默共享，也不能使用 unsafe 改写私有状态。
		result.Set(value)
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if !field.IsExported() || field.Tag.Get("mapstructure") == "-" {
				if !canShallowCopyDefault(value.Field(i), depth+1) {
					return reflect.Value{}, fmt.Errorf("config default field %s.%s is ignored or unexported and cannot safely retain mutable references or runtime resources", value.Type(), field.Name)
				}
				continue
			}
			element, err := cloneDefaultValue(value.Field(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(i).Set(element)
		}
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return reflect.Value{}, errors.New("config defaults must contain data, not runtime resources")
	default:
		result.Set(value)
	}
	return result, nil
}

// 不解包指针，不读取字段 Interface，也不依赖 CanSet：检查同样适用于
// 未导出字段。接口、结构体和数组可能在值类型内部隐藏引用，须继续检查。
// nil 引用没有可共享的对象，可以保留；非 nil 引用一律拒绝。
func canShallowCopyDefault(value reflect.Value, depth int) bool {
	if depth > maxDefaultDepth {
		return false
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return value.IsZero()
	case reflect.Interface:
		return value.IsNil() || canShallowCopyDefault(value.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if !canShallowCopyDefault(value.Field(i), depth+1) {
				return false
			}
		}
	case reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if !canShallowCopyDefault(value.Index(i), depth+1) {
				return false
			}
		}
	}
	return true
}
