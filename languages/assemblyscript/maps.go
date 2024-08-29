/*
 * Copyright 2024 Hypermode, Inc.
 */

package assemblyscript

import (
	"context"
	"fmt"
	"reflect"

	"hmruntime/languages/assemblyscript/hash"
	"hmruntime/utils"
)

// Reference: https://github.com/AssemblyScript/assemblyscript/blob/main/std/assembly/map.ts

func (wa *wasmAdapter) readMap(ctx context.Context, typ string, offset uint32) (data any, err error) {

	mem := wa.mod.Memory()

	// buckets, ok := mem.ReadUint32Le(offset)
	// if !ok {
	// 	return nil, fmt.Errorf("failed to read map buckets pointer")
	// }

	// bucketsMask, ok := mem.ReadUint32Le(offset + 4)
	// if !ok {
	// 	return nil, fmt.Errorf("failed to read map buckets mask")
	// }

	entries, ok := mem.ReadUint32Le(offset + 8)
	if !ok {
		return nil, fmt.Errorf("failed to read map entries pointer")
	}

	entriesCapacity, ok := mem.ReadUint32Le(offset + 12)
	if !ok {
		return nil, fmt.Errorf("failed to read map entries capacity")
	}

	// entriesOffset, ok := mem.ReadUint32Le(offset + 16)
	// if !ok {
	// 	return nil, fmt.Errorf("failed to read map entries offset")
	// }

	entriesCount, ok := mem.ReadUint32Le(offset + 20)
	if !ok {
		return nil, fmt.Errorf("failed to read map entries count")
	}

	// the length of array buffer is stored 4 bytes before the offset
	byteLength, ok := mem.ReadUint32Le(entries - 4)
	if !ok {
		return nil, fmt.Errorf("failed to read map entries buffer length")
	}

	entrySize := byteLength / entriesCapacity
	keyType, valueType := wa.typeInfo.GetMapSubtypes(typ)
	valueOffset := getSizeForOffset(keyType)

	rKeyType, err := wa.getReflectedType(ctx, keyType)
	if err != nil {
		return nil, err
	}
	rValueType, err := wa.getReflectedType(ctx, valueType)
	if err != nil {
		return nil, err
	}

	size := int(entriesCount)

	if rKeyType.Comparable() {
		// return a map
		m := reflect.MakeMapWithSize(reflect.MapOf(rKeyType, rValueType), size)
		for i := uint32(0); i < entriesCount; i++ {
			p := entries + (i * entrySize)
			k, err := wa.readField(ctx, keyType, p)
			if err != nil {
				return nil, err
			}
			v, err := wa.readField(ctx, valueType, p+valueOffset)
			if err != nil {
				return nil, err
			}
			m.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(v))
		}
		return m.Interface(), nil

	} else {
		// return a pseudo-map
		sliceType := reflect.SliceOf(reflect.StructOf([]reflect.StructField{
			{
				Name: "Key",
				Type: rKeyType,
				Tag:  `json:"key"`,
			},
			{
				Name: "Value",
				Type: rValueType,
				Tag:  `json:"value"`,
			},
		}))
		s := reflect.MakeSlice(sliceType, size, size)
		for i := 0; i < size; i++ {
			p := entries + uint32(i)*entrySize
			k, err := wa.readField(ctx, keyType, p)
			if err != nil {
				return nil, err
			}
			v, err := wa.readField(ctx, valueType, p+valueOffset)
			if err != nil {
				return nil, err
			}
			s.Index(i).Field(0).Set(reflect.ValueOf(k))
			s.Index(i).Field(1).Set(reflect.ValueOf(v))
		}
		t := reflect.StructOf([]reflect.StructField{
			{
				Name: "Data",
				Type: sliceType,
				Tag:  `json:"$mapdata"`,
			},
		})
		w := reflect.New(t).Elem()
		w.Field(0).Set(s)
		return w.Interface(), nil
	}
}

func (wa *wasmAdapter) writeMap(ctx context.Context, typ string, data any) (offset uint32, err error) {

	// Unfortunately, there's no way to do this without reflection.
	rv := reflect.ValueOf(data)
	if rv.Kind() != reflect.Map {
		// TODO: support []kvp ?
		return 0, fmt.Errorf("unsupported map type %T", data)
	}
	mapLen := uint32(rv.Len())

	// unpin everything when done
	var pins = make([]uint32, 0, (mapLen*2)+2)
	defer func() {
		for _, ptr := range pins {
			err = wa.unpinWasmMemory(ctx, ptr)
			if err != nil {
				break
			}
		}
	}()

	// determine capacities and mask
	bucketsCapacity := uint32(4)
	entriesCapacity := uint32(4)
	bucketsMask := bucketsCapacity - 1
	for bucketsCapacity < mapLen {
		bucketsCapacity <<= 1
		entriesCapacity = bucketsCapacity * 8 / 3
		bucketsMask = bucketsCapacity - 1
	}

	// create buckets array buffer
	const bucketSize = 4
	bucketsBufferSize := bucketSize * bucketsCapacity
	bucketsBufferOffset, err := wa.allocateWasmMemory(ctx, bucketsBufferSize, 1)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate memory for array buffer: %w", err)
	}

	// pin the array buffer so it can't get garbage collected
	// when we allocate the array object
	err = wa.pinWasmMemory(ctx, bucketsBufferOffset)
	if err != nil {
		return 0, fmt.Errorf("failed to pin array buffer: %w", err)
	}
	pins = append(pins, bucketsBufferOffset)

	// write entries array buffer
	// note: unlike arrays, an empty map DOES have array buffers
	keyType, valueType := wa.typeInfo.GetMapSubtypes(typ)
	keySize, _ := wa.typeInfo.GetSizeOfType(ctx, keyType)
	valueSize, _ := wa.typeInfo.GetSizeOfType(ctx, valueType)
	const taggedNextSize = 4
	entryAlign := max(keySize, valueSize, 4) - 1
	entrySize := (keySize + valueSize + taggedNextSize + entryAlign) & ^entryAlign
	entriesBufferSize := entrySize * entriesCapacity
	entriesBufferOffset, err := wa.allocateWasmMemory(ctx, entriesBufferSize, 1)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate memory for array buffer: %w", err)
	}

	// pin the array buffer so it can't get garbage collected
	// when we allocate the array object
	err = wa.pinWasmMemory(ctx, entriesBufferOffset)
	if err != nil {
		return 0, fmt.Errorf("failed to pin array buffer: %w", err)
	}
	pins = append(pins, entriesBufferOffset)

	valueOffset := getSizeForOffset(keyType)
	taggedNextOffset := getSizeForOffset(valueType) + valueOffset

	mem := wa.mod.Memory()
	mapKeys := rv.MapKeys()
	for i, mapKey := range mapKeys {

		entryOffset := entriesBufferOffset + (entrySize * uint32(i))

		// write entry key and calculate hash code
		var hashCode, ptr uint32
		key := mapKey.Interface()
		switch t := key.(type) {
		case string:
			// Special case for string keys.  Since we need to encode them as UTF16,
			// for both writing to memory and calculating the hash code, bypass the
			// normal writeField/writeString functions and do it manually.
			bytes := utils.EncodeUTF16(t)
			hashCode = hash.GetHashCode(bytes)
			ptr, err = wa.writeRawBytes(ctx, bytes, 2)
			if err != nil {
				return 0, fmt.Errorf("failed to write map entry key: %w", err)
			}
			ok := mem.WriteUint32Le(entryOffset, ptr)
			if !ok {
				return 0, fmt.Errorf("failed to write map entry key pointer")
			}

		default:
			hashCode = hash.GetHashCode(key)
			ptr, err = wa.writeField(ctx, keyType, entryOffset, key)
			if err != nil {
				return 0, fmt.Errorf("failed to write map entry key: %w", err)
			}
		}

		// If we allocated memory for the key, we need to pin it too.
		if ptr != 0 {
			err = wa.pinWasmMemory(ctx, ptr)
			if err != nil {
				return 0, err
			}
			pins = append(pins, ptr)
		}

		// write entry value
		mapValue := rv.MapIndex(mapKey)
		value := mapValue.Interface()
		entryValueOffset := entryOffset + valueOffset
		ptr, err = wa.writeField(ctx, valueType, entryValueOffset, value)
		if err != nil {
			return 0, fmt.Errorf("failed to write map entry value: %w", err)
		}

		// If we allocated memory for the value, we need to pin it too.
		if ptr != 0 {
			err = wa.pinWasmMemory(ctx, ptr)
			if err != nil {
				return 0, err
			}
			pins = append(pins, ptr)
		}

		// write to bucket and "tagged next" field
		bucketPtrBase := bucketsBufferOffset + ((hashCode & bucketsMask) * bucketSize)
		prev, ok := mem.ReadUint32Le(bucketPtrBase)
		if !ok {
			return 0, fmt.Errorf("failed to read previous map entry bucket pointer")
		}
		ok = mem.WriteUint32Le(entryOffset+taggedNextOffset, prev)
		if !ok {
			return 0, fmt.Errorf("failed to write map entry tagged next field")
		}
		ok = mem.WriteUint32Le(bucketPtrBase, entryOffset)
		if !ok {
			return 0, fmt.Errorf("failed to write map entry bucket pointer")
		}
	}

	def, err := wa.typeInfo.GetTypeDefinition(ctx, typ)
	if err != nil {
		return 0, err
	}

	// write map object
	const size = 24
	offset, err = wa.allocateWasmMemory(ctx, size, def.Id)
	if err != nil {
		return 0, err
	}

	ok := mem.WriteUint32Le(offset, bucketsBufferOffset)
	if !ok {
		return 0, fmt.Errorf("failed to write map buckets pointer")
	}

	ok = mem.WriteUint32Le(offset+4, bucketsMask)
	if !ok {
		return 0, fmt.Errorf("failed to write map buckets mask")
	}

	ok = mem.WriteUint32Le(offset+8, entriesBufferOffset)
	if !ok {
		return 0, fmt.Errorf("failed to write map entries pointer")
	}

	ok = mem.WriteUint32Le(offset+12, entriesCapacity)
	if !ok {
		return 0, fmt.Errorf("failed to write map entries capacity")
	}

	ok = mem.WriteUint32Le(offset+16, mapLen)
	if !ok {
		return 0, fmt.Errorf("failed to write map entries offset")
	}

	ok = mem.WriteUint32Le(offset+20, mapLen)
	if !ok {
		return 0, fmt.Errorf("failed to write map entries count")
	}

	return offset, nil
}

func getSizeForOffset(typ string) uint32 {
	switch typ {
	case "u64", "i64", "f64":
		// 64-bit keys have 8-byte value offset
		return 8
	default:
		// everything else has 4-byte value offset
		return 4
	}
}
