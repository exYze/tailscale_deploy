// Command machofat combines thin Mach-O executables (e.g. darwin/amd64 and
// darwin/arm64) into one universal ("fat") binary, like Apple's `lipo -create`,
// so a single macOS file runs natively on both Intel and Apple silicon Macs.
//
// Usage: go run ./tools/machofat <output> <input>...
package main

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	fatMagic  = 0xCAFEBABE
	alignLog2 = 14 // 16 KiB, the arm64 page size, as used by lipo
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: machofat <output> <input> <input>...")
		os.Exit(2)
	}
	if err := combine(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "machofat:", err)
		os.Exit(1)
	}
}

func combine(out string, inputs []string) error {
	type slice struct {
		data        []byte
		cpu, subcpu uint32
	}
	slices := make([]slice, 0, len(inputs))
	seen := map[uint32]bool{}
	for _, in := range inputs {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("%s: not a thin Mach-O file: %w", in, err)
		}
		if seen[uint32(f.Cpu)] {
			return fmt.Errorf("%s: duplicate CPU type %v", in, f.Cpu)
		}
		seen[uint32(f.Cpu)] = true
		slices = append(slices, slice{data: data, cpu: uint32(f.Cpu), subcpu: f.SubCpu})
	}

	align := uint32(1) << alignLog2
	alignUp := func(n uint32) uint32 { return (n + align - 1) &^ (align - 1) }

	var buf bytes.Buffer
	be := binary.BigEndian
	binary.Write(&buf, be, uint32(fatMagic))
	binary.Write(&buf, be, uint32(len(slices)))
	offset := alignUp(uint32(8 + 20*len(slices)))
	offsets := make([]uint32, len(slices))
	for i, s := range slices {
		offsets[i] = offset
		binary.Write(&buf, be, s.cpu)
		binary.Write(&buf, be, s.subcpu)
		binary.Write(&buf, be, offset)
		binary.Write(&buf, be, uint32(len(s.data)))
		binary.Write(&buf, be, uint32(alignLog2))
		offset = alignUp(offset + uint32(len(s.data)))
	}
	for i, s := range slices {
		buf.Write(make([]byte, int(offsets[i])-buf.Len())) // padding
		buf.Write(s.data)
	}
	return os.WriteFile(out, buf.Bytes(), 0o755)
}
