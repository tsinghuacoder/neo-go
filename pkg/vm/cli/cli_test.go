package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	gio "io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abiosoft/readline"
	"github.com/nspcc-dev/neo-go/pkg/compiler"
	"github.com/nspcc-dev/neo-go/pkg/config"
	"github.com/nspcc-dev/neo-go/pkg/core/interop/interopnames"
	"github.com/nspcc-dev/neo-go/pkg/io"
	"github.com/nspcc-dev/neo-go/pkg/smartcontract/nef"
	"github.com/nspcc-dev/neo-go/pkg/util"
	"github.com/nspcc-dev/neo-go/pkg/vm"
	"github.com/nspcc-dev/neo-go/pkg/vm/emit"
	"github.com/nspcc-dev/neo-go/pkg/vm/opcode"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

type readCloser struct {
	sync.Mutex
	bytes.Buffer
}

func (r *readCloser) Close() error {
	return nil
}

func (r *readCloser) Read(p []byte) (int, error) {
	r.Lock()
	defer r.Unlock()
	return r.Buffer.Read(p)
}

func (r *readCloser) WriteString(s string) {
	r.Lock()
	defer r.Unlock()
	r.Buffer.WriteString(s)
}

type executor struct {
	in   *readCloser
	out  *bytes.Buffer
	cli  *VMCLI
	ch   chan struct{}
	exit atomic.Bool
}

func newTestVMCLI(t *testing.T) *executor {
	return newTestVMCLIWithLogo(t, false)
}

func newTestVMCLIWithLogo(t *testing.T, printLogo bool) *executor {
	e := &executor{
		in:  &readCloser{Buffer: *bytes.NewBuffer(nil)},
		out: bytes.NewBuffer(nil),
		ch:  make(chan struct{}),
	}
	e.cli = NewWithConfig(printLogo,
		func(int) { e.exit.Store(true) },
		&readline.Config{
			Prompt: "",
			Stdin:  e.in,
			Stdout: e.out,
		})
	return e
}

func (e *executor) runProg(t *testing.T, commands ...string) {
	cmd := strings.Join(commands, "\n") + "\n"
	e.in.WriteString(cmd + "\n")
	go func() {
		require.NoError(t, e.cli.Run())
		close(e.ch)
	}()
	select {
	case <-e.ch:
	case <-time.After(time.Second):
		require.Fail(t, "command took too long time")
	}
}

func (e *executor) checkNextLine(t *testing.T, expected string) {
	line, err := e.out.ReadString('\n')
	require.NoError(t, err)
	require.Regexp(t, expected, line)
}

func (e *executor) checkError(t *testing.T, expectedErr error) {
	line, err := e.out.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "Error: "+expectedErr.Error()))
}

func (e *executor) checkStack(t *testing.T, items ...interface{}) {
	d := json.NewDecoder(e.out)
	var actual interface{}
	require.NoError(t, d.Decode(&actual))
	rawActual, err := json.Marshal(actual)
	require.NoError(t, err)

	expected := vm.NewStack("")
	for i := range items {
		expected.PushVal(items[i])
	}
	rawExpected, err := json.Marshal(expected)
	require.NoError(t, err)
	require.JSONEq(t, string(rawExpected), string(rawActual))

	// Decoder has it's own buffer, we need to return unread part to the output.
	outRemain := e.out.String()
	e.out.Reset()
	_, err = gio.Copy(e.out, d.Buffered())
	require.NoError(t, err)
	e.out.WriteString(outRemain)
	_, err = e.out.ReadString('\n')
	require.NoError(t, err)
}

func TestLoad(t *testing.T) {
	script := []byte{byte(opcode.PUSH3), byte(opcode.PUSH4), byte(opcode.ADD)}
	t.Run("loadhex", func(t *testing.T) {
		e := newTestVMCLI(t)
		e.runProg(t,
			"loadhex",
			"loadhex notahex",
			"loadhex "+hex.EncodeToString(script))

		e.checkError(t, ErrMissingParameter)
		e.checkError(t, ErrInvalidParameter)
		e.checkNextLine(t, "READY: loaded 3 instructions")
	})
	t.Run("loadbase64", func(t *testing.T) {
		e := newTestVMCLI(t)
		e.runProg(t,
			"loadbase64",
			"loadbase64 not_a_base64",
			"loadbase64 "+base64.StdEncoding.EncodeToString(script))

		e.checkError(t, ErrMissingParameter)
		e.checkError(t, ErrInvalidParameter)
		e.checkNextLine(t, "READY: loaded 3 instructions")
	})

	src := `package kek
	func Main(op string, a, b int) int {
		if op == "add" {
			return a + b
		} else {
			return a * b
		}
	}`
	tmpDir := path.Join(os.TempDir(), "vmcliloadtest")
	require.NoError(t, os.Mkdir(tmpDir, os.ModePerm))
	t.Cleanup(func() {
		os.RemoveAll(tmpDir)
	})

	t.Run("loadgo", func(t *testing.T) {
		filename := path.Join(tmpDir, "vmtestcontract.go")
		require.NoError(t, ioutil.WriteFile(filename, []byte(src), os.ModePerm))
		filenameErr := path.Join(tmpDir, "vmtestcontract_err.go")
		require.NoError(t, ioutil.WriteFile(filenameErr, []byte(src+"invalid_token"), os.ModePerm))

		e := newTestVMCLI(t)
		e.runProg(t,
			"loadgo",
			"loadgo "+filenameErr,
			"loadgo "+filename,
			"run main add 3 5")

		e.checkError(t, ErrMissingParameter)
		e.checkNextLine(t, "Error:")
		e.checkNextLine(t, "READY: loaded \\d* instructions")
		e.checkStack(t, 8)
	})
	t.Run("loadnef", func(t *testing.T) {
		config.Version = "0.92.0-test"

		script, di, err := compiler.CompileWithDebugInfo("test", strings.NewReader(src))
		require.NoError(t, err)
		nefFile, err := nef.NewFile(script)
		require.NoError(t, err)
		filename := path.Join(tmpDir, "vmtestcontract.nef")
		rawNef, err := nefFile.Bytes()
		require.NoError(t, err)
		require.NoError(t, ioutil.WriteFile(filename, rawNef, os.ModePerm))
		m, err := di.ConvertToManifest(&compiler.Options{})
		require.NoError(t, err)
		manifestFile := path.Join(tmpDir, "vmtestcontract.manifest.json")
		rawManifest, err := json.Marshal(m)
		require.NoError(t, err)
		require.NoError(t, ioutil.WriteFile(manifestFile, rawManifest, os.ModePerm))
		filenameErr := path.Join(tmpDir, "vmtestcontract_err.nef")
		require.NoError(t, ioutil.WriteFile(filenameErr, append([]byte{1, 2, 3, 4}, rawNef...), os.ModePerm))
		notExists := path.Join(tmpDir, "notexists.json")

		e := newTestVMCLI(t)
		e.runProg(t,
			"loadnef",
			"loadnef "+filenameErr+" "+manifestFile,
			"loadnef "+filename+" "+notExists,
			"loadnef "+filename+" "+filename,
			"loadnef "+filename+" "+manifestFile,
			"run main add 3 5")

		e.checkError(t, ErrMissingParameter)
		e.checkNextLine(t, "Error:")
		e.checkNextLine(t, "Error:")
		e.checkNextLine(t, "Error:")
		e.checkNextLine(t, "READY: loaded \\d* instructions")
		e.checkStack(t, 8)
	})
}

func TestRunWithDifferentArguments(t *testing.T) {
	src := `package kek
	var a = 1
	func init() {
		a += 1
	}
	func InitHasRun() bool {
		return a == 2
	}
	func Negate(arg bool) bool {
		return !arg
	}
	func GetInt(arg int) int {
		return arg
	}
	func GetString(arg string) string {
		return arg
	}`

	filename := path.Join(os.TempDir(), "run_vmtestcontract.go")
	require.NoError(t, ioutil.WriteFile(filename, []byte(src), os.ModePerm))
	t.Cleanup(func() {
		os.Remove(filename)
	})

	e := newTestVMCLI(t)
	e.runProg(t,
		"loadgo "+filename, "run notexists",
		"loadgo "+filename, "run negate false",
		"loadgo "+filename, "run negate true",
		"loadgo "+filename, "run negate bool:invalid",
		"loadgo "+filename, "run getInt 123",
		"loadgo "+filename, "run getInt int:invalid",
		"loadgo "+filename, "run getString validstring",
		"loadgo "+filename, "run initHasRun",
		"loadhex "+hex.EncodeToString([]byte{byte(opcode.ADD)}),
		"run _ 1 2",
		"loadbase64 "+base64.StdEncoding.EncodeToString([]byte{byte(opcode.MUL)}),
		"run _ 21 2",
	)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkNextLine(t, "Error:")

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, true)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, false)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkError(t, ErrInvalidParameter)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, 123)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkError(t, ErrInvalidParameter)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, "validstring")

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, true)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, 3)

	e.checkNextLine(t, "READY: loaded \\d.* instructions")
	e.checkStack(t, 42)
}

func TestPrintOps(t *testing.T) {
	w := io.NewBufBinWriter()
	emit.Opcodes(w.BinWriter, opcode.PUSH1)
	emit.Syscall(w.BinWriter, interopnames.SystemBinarySerialize)
	emit.Instruction(w.BinWriter, opcode.PUSHDATA1, []byte{3, 1, 2, 3})
	script := w.Bytes()
	e := newTestVMCLI(t)
	e.runProg(t,
		"ops",
		"loadhex "+hex.EncodeToString(script),
		"ops")

	e.checkNextLine(t, ".*no program loaded")
	e.checkNextLine(t, fmt.Sprintf("READY: loaded %d instructions", len(script)))
	e.checkNextLine(t, "INDEX.*OPCODE.*PARAMETER")
	e.checkNextLine(t, "0.*PUSH1")
	e.checkNextLine(t, "1.*SYSCALL.*System\\.Binary\\.Serialize")
	e.checkNextLine(t, "6.*PUSHDATA1.*010203")
}

func TestLoadAbort(t *testing.T) {
	e := newTestVMCLI(t)
	e.runProg(t,
		"loadhex "+hex.EncodeToString([]byte{byte(opcode.PUSH1), byte(opcode.ABORT)}),
		"run",
	)

	e.checkNextLine(t, fmt.Sprintf("READY: loaded 2 instructions"))
	e.checkNextLine(t, "Error:.*at instruction 1.*ABORT")
}

func TestBreakpoint(t *testing.T) {
	w := io.NewBufBinWriter()
	emit.Opcodes(w.BinWriter, opcode.PUSH1, opcode.PUSH2, opcode.ADD, opcode.PUSH6, opcode.ADD)
	e := newTestVMCLI(t)
	e.runProg(t,
		"break 3",
		"cont",
		"ip",
		"loadhex "+hex.EncodeToString(w.Bytes()),
		"break",
		"break second",
		"break 2",
		"break 4",
		"cont", "estack",
		"run", "estack",
		"cont",
	)

	e.checkNextLine(t, "no program loaded")
	e.checkNextLine(t, "no program loaded")
	e.checkNextLine(t, "no program loaded")
	e.checkNextLine(t, fmt.Sprintf("READY: loaded 5 instructions"))
	e.checkError(t, ErrMissingParameter)
	e.checkError(t, ErrInvalidParameter)
	e.checkNextLine(t, "breakpoint added at instruction 2")
	e.checkNextLine(t, "breakpoint added at instruction 4")

	e.checkNextLine(t, "at breakpoint 2.*ADD")
	e.checkStack(t, 1, 2)

	e.checkNextLine(t, "at breakpoint 4.*ADD")
	e.checkStack(t, 3, 6)

	e.checkStack(t, 9)
}

func TestStep(t *testing.T) {
	script := hex.EncodeToString([]byte{
		byte(opcode.PUSH0), byte(opcode.PUSH1), byte(opcode.PUSH2), byte(opcode.PUSH3),
	})
	e := newTestVMCLI(t)
	e.runProg(t,
		"step",
		"loadhex "+script,
		"step invalid",
		"step",
		"step 2",
		"ip", "step", "ip")

	e.checkNextLine(t, "no program loaded")
	e.checkNextLine(t, "READY: loaded \\d+ instructions")
	e.checkError(t, ErrInvalidParameter)
	e.checkNextLine(t, "at breakpoint 1.*PUSH1")
	e.checkNextLine(t, "at breakpoint 3.*PUSH3")
	e.checkNextLine(t, "instruction pointer at 3.*PUSH3")
	e.checkNextLine(t, "execution has finished")
	e.checkNextLine(t, "execution has finished")
}

func TestErrorOnStepInto(t *testing.T) {
	script := hex.EncodeToString([]byte{byte(opcode.ADD)})
	e := newTestVMCLI(t)
	e.runProg(t,
		"stepover",
		"loadhex "+script,
		"stepover")

	e.checkNextLine(t, "Error:.*no program loaded")
	e.checkNextLine(t, "READY: loaded 1 instructions")
	e.checkNextLine(t, "Error:")
}

func TestStepIntoOverOut(t *testing.T) {
	script := hex.EncodeToString([]byte{
		byte(opcode.PUSH2), byte(opcode.CALL), 4, byte(opcode.NOP), byte(opcode.RET),
		byte(opcode.PUSH3), byte(opcode.ADD), byte(opcode.RET),
	})

	e := newTestVMCLI(t)
	e.runProg(t,
		"loadhex "+script,
		"step", "stepover", "run",
		"loadhex "+script,
		"step", "stepinto", "step", "estack", "run",
		"loadhex "+script,
		"step", "stepinto", "stepout", "run")

	e.checkNextLine(t, fmt.Sprintf("READY: loaded \\d+ instructions"))
	e.checkNextLine(t, "at breakpoint 1.*CALL")
	e.checkNextLine(t, "instruction pointer at.*NOP")
	e.checkStack(t, 5)

	e.checkNextLine(t, fmt.Sprintf("READY: loaded \\d+ instructions"))
	e.checkNextLine(t, "at breakpoint.*CALL")
	e.checkNextLine(t, "instruction pointer at.*PUSH3")
	e.checkNextLine(t, "at breakpoint.*ADD")
	e.checkStack(t, 2, 3)
	e.checkStack(t, 5)

	e.checkNextLine(t, fmt.Sprintf("READY: loaded \\d+ instructions"))
	e.checkNextLine(t, "at breakpoint 1.*CALL")
	e.checkNextLine(t, "instruction pointer at.*PUSH3")
	e.checkNextLine(t, "instruction pointer at.*NOP")
	e.checkStack(t, 5)
}

// `Parse` output is written via `tabwriter` so if any problems
// are encountered in this test, try to replace ' ' with '\\s+'.
func TestParse(t *testing.T) {
	t.Run("Integer", func(t *testing.T) {
		e := newTestVMCLI(t)
		e.runProg(t,
			"parse",
			"parse 6667")

		e.checkError(t, ErrMissingParameter)
		e.checkNextLine(t, "Integer to Hex.*0b1a")
		e.checkNextLine(t, "Integer to Base64.*Cxo=")
		e.checkNextLine(t, "Hex to String.*\"fg\"")
		e.checkNextLine(t, "Hex to Integer.*26470")
		e.checkNextLine(t, "Swap Endianness.*6766")
		e.checkNextLine(t, "Base64 to String.*\"뮻\"")
		e.checkNextLine(t, "Base64 to BigInteger.*-4477205")
		e.checkNextLine(t, "String to Hex.*36363637")
		e.checkNextLine(t, "String to Base64.*NjY2Nw==")
	})
	t.Run("Address", func(t *testing.T) {
		e := newTestVMCLI(t)
		e.runProg(t, "parse "+"NbTiM6h8r99kpRtb428XcsUk1TzKed2gTc")
		e.checkNextLine(t, "Address to BE ScriptHash.*aa8acf859d4fe402b34e673f2156821796a488eb")
		e.checkNextLine(t, "Address to LE ScriptHash.*eb88a496178256213f674eb302e44f9d85cf8aaa")
		e.checkNextLine(t, "Address to Base64.*(BE).*qorPhZ1P5AKzTmc/IVaCF5akiOs=")
		e.checkNextLine(t, "Address to Base64.*(LE).*64iklheCViE/Z06zAuRPnYXPiqo=")
		e.checkNextLine(t, "String to Hex.*4e6254694d3668387239396b70527462343238586373556b31547a4b656432675463")
		e.checkNextLine(t, "String to Base64.*TmJUaU02aDhyOTlrcFJ0YjQyOFhjc1VrMVR6S2VkMmdUYw==")
	})
	t.Run("Uint160", func(t *testing.T) {
		u := util.Uint160{66, 67, 68}
		e := newTestVMCLI(t)
		e.runProg(t, "parse "+u.StringLE())
		e.checkNextLine(t, "Integer to Hex.*b6c706")
		e.checkNextLine(t, "Integer to Base64.*tscG")
		e.checkNextLine(t, "BE ScriptHash to Address.*NKuyBkoGdZZSLyPbJEetheRhQKhATAzN2A")
		e.checkNextLine(t, "LE ScriptHash to Address.*NRxLN7apYwKJihzMt4eSSnU9BJ77dp2TNj")
		e.checkNextLine(t, "Hex to String")
		e.checkNextLine(t, "Hex to Integer.*378293464438118320046642359484100328446970822656")
		e.checkNextLine(t, "Swap Endianness.*4243440000000000000000000000000000000000")
		e.checkNextLine(t, "Base64 to String.*")
		e.checkNextLine(t, "Base64 to BigInteger.*376115185060690908522683414825349447309891933036899526770189324554358227")
		e.checkNextLine(t, "String to Hex.*30303030303030303030303030303030303030303030303030303030303030303030343434333432")
		e.checkNextLine(t, "String to Base64.*MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDQ0NDM0Mg==")
	})
}

func TestPrintLogo(t *testing.T) {
	e := newTestVMCLIWithLogo(t, true)
	e.runProg(t)
	require.True(t, strings.HasPrefix(e.out.String(), logo))
	require.False(t, e.exit.Load())
}

func TestExit(t *testing.T) {
	e := newTestVMCLI(t)
	e.runProg(t, "exit")
	require.True(t, e.exit.Load())
}
