package modbus

import (
	"errors"
	"sync"
)

// Reg defines a Modbus register
type Reg struct {
	Address  uint16
	Value    uint16
	Validate func(value uint16) bool
}

// RegProvider is the interface for a register provider.
// Regs is the canonical implementation.
type RegProvider interface {
	ReadReg(address int) (uint16, error)
	WriteReg(address int, value uint16) error
	ReadInputReg(address int) (uint16, error)
	ReadDiscreteInput(num int) (bool, error)
	ReadCoil(num int) (bool, error)
	WriteCoil(num int, value bool) error
}

// Regs represents all registers in a modbus device and provides functions
// to read/write 16-bit and bit values. This register module assumes all
// register types map into one address space
// as described in the modbus spec
// (http://www.modbus.org/docs/Modbus_Application_Protocol_V1_1b3.pdf)
// on page 6 and 7.
// All operations on Regs are threadsafe and protected by a mutex.
type Regs struct {
	regs []Reg
	lock sync.RWMutex
}

// AddReg is used to add a modbus register to the server.
// the callback function is called when the reg is updated
// The register can be updated by word or bit operations.
func (r *Regs) AddReg(address int, count int) {
	r.lock.Lock()
	defer r.lock.Unlock()
	// first check if reg already exists
	for i := 0; i < count; i++ {
		found := false
		adr := address + i
		for _, reg := range r.regs {
			if reg.Address == uint16(adr) {
				found = true
			}
		}
		if !found {
			r.regs = append(r.regs, Reg{uint16(adr), 0, nil})
		}
	}
}

// ErrUnknownRegister is returned if a validator is added on a register that has
// not been added.
var ErrUnknownRegister = errors.New("unknown register")

// AddRegValueValidator is used to add a validator function to a modbus register.
// The validator function is called when a modbus client tries to write a value.
// If the value is invalid, ExcIllegalValue (modbus exception 3) is returned to
// the client.
func (r *Regs) AddRegValueValidator(address int, validate func(uint16) bool) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	for ix := range r.regs {
		if r.regs[ix].Address == uint16(address) {
			r.regs[ix].Validate = validate
			return nil
		}
	}
	return ErrUnknownRegister
}

func (r *Regs) readReg(address int) (uint16, error) {
	for _, reg := range r.regs {
		if reg.Address == uint16(address) {
			return reg.Value, nil
		}
	}

	return 0, ExcIllegalAddress
}

// ReadReg is used to read a modbus holding register
func (r *Regs) ReadReg(address int) (uint16, error) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	v, err := r.readReg(address)
	return v, err
}

// ReadInputReg is used to read a modbus input register
func (r *Regs) ReadInputReg(address int) (uint16, error) {
	return r.ReadReg(address)
}

func (r *Regs) writeReg(address int, value uint16) error {
	for i, reg := range r.regs {
		if reg.Address == uint16(address) {
			// if a validator is present, check if the value is allowed
			if reg.Validate != nil && !reg.Validate(value) {
				return ExcIllegalValue
			}
			(r.regs)[i].Value = value
			return nil
		}
	}

	return ExcIllegalAddress
}

// WriteReg is used to write a modbus register
func (r *Regs) WriteReg(address int, value uint16) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.writeReg(address, value)
}

// AddCoil is used to add a discrete io to the register map.
// Note coils are aliased on top of other registers, so coil 20
// would be register 1 bit 4 (16 + 4 = 20).
func (r *Regs) AddCoil(num int) {
	regAddress := num / 16
	r.AddReg(regAddress, 1)
}

// ReadCoil gets a coil value
func (r *Regs) ReadCoil(num int) (bool, error) {
	regAddress := (num / 16)
	regValue, err := r.ReadReg(regAddress)
	if err != nil {
		return false, err
	}

	bitPos := uint16(num % 16)
	ret := (regValue & (1 << bitPos)) != 0
	return ret, nil
}

// ReadDiscreteInput gets a discrete input
func (r *Regs) ReadDiscreteInput(num int) (bool, error) {
	return r.ReadCoil(num)
}

// WriteCoil writes a coil value
func (r *Regs) WriteCoil(num int, value bool) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	regAddress := (num / 16)
	regValue, err := r.readReg(regAddress)
	if err != nil {
		return err
	}

	bitPos := uint16(num % 16)

	if value {
		regValue |= 1 << bitPos
	} else {
		regValue &= ^(1 << bitPos)
	}

	return r.writeReg(regAddress, regValue)
}

// ReadRegUint32 reads a uint32 from regs
func (r *Regs) ReadRegUint32(address int) (uint32, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := make([]uint16, 2)

	var err error
	regs[0], err = r.readReg(address)
	if err != nil {
		return 0, err
	}
	regs[1], err = r.readReg(address + 1)
	if err != nil {
		return 0, err
	}

	return RegsToUint32(regs)[0], nil
}

// WriteRegUint32 writes a uint32 to regs
func (r *Regs) WriteRegUint32(address int, value uint32) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := Uint32ToRegs([]uint32{value})

	for i, reg := range regs {
		err := r.writeReg(address+i, reg)
		if err != nil {
			return err
		}
	}

	return nil
}

// ReadRegInt32 reads a int32 from regs
func (r *Regs) ReadRegInt32(address int) (int32, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := make([]uint16, 2)

	var err error
	regs[0], err = r.readReg(address)
	if err != nil {
		return 0, err
	}
	regs[1], err = r.readReg(address + 1)
	if err != nil {
		return 0, err
	}

	return RegsToInt32(regs)[0], nil
}

// WriteRegInt32 writes a int32 to regs
func (r *Regs) WriteRegInt32(address int, value int32) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := Int32ToRegs([]int32{value})

	for i, reg := range regs {
		err := r.writeReg(address+i, reg)
		if err != nil {
			return err
		}
	}

	return nil
}

// ReadRegFloat32 reads a float32 from regs
func (r *Regs) ReadRegFloat32(address int) (float32, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := make([]uint16, 2)

	var err error
	regs[0], err = r.readReg(address)
	if err != nil {
		return 0, err
	}
	regs[1], err = r.readReg(address + 1)
	if err != nil {
		return 0, err
	}

	return RegsToFloat32(regs)[0], nil
}

// WriteRegFloat32 writes a float32 to regs
func (r *Regs) WriteRegFloat32(address int, value float32) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	regs := Float32ToRegs([]float32{value})

	for i, reg := range regs {
		err := r.writeReg(address+i, reg)
		if err != nil {
			return err
		}
	}

	return nil
}
