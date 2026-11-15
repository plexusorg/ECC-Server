package model

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The OpenVINO runtime, called through its C API without cgo: purego loads the shared libraries at run time, so the
// binary still builds with CGO_ENABLED=0. The Debian package installs the libraries in RuntimeDir.

// RuntimeDir holds OpenVINO's shared libraries: libtbb.so.12, libopenvino.so.<n>, libopenvino_c.so.<n>,
// libopenvino_ir_frontend.so.<n> and libopenvino_intel_cpu_plugin.so.
const RuntimeDir = "/usr/lib/ecc-server/openvino"

// runtimeSoname is the soname suffix of the OpenVINO release the package installs (2026.4.1).
const runtimeSoname = "2641"

const (
	elemF32 = 4  // ov_element_type_e F32
	elemI64 = 10 // ov_element_type_e I64
)

var (
	loadOnce sync.Once
	loadErr  error

	ovCoreCreate     func(core *uintptr) int32
	ovCoreFree       func(core uintptr)
	ovReadModel      func(core uintptr, xml string, bin uintptr, model *uintptr) int32
	ovCompileProps   func(core, model uintptr, device string, n uintptr, props unsafe.Pointer, out *uintptr) int32
	ovModelFree      func(model uintptr)
	ovCompiledFree   func(compiled uintptr)
	ovCreateRequest  func(compiled uintptr, req *uintptr) int32
	ovRequestFree    func(req uintptr)
	ovTensorFromHost func(typ int32, rank int64, dims unsafe.Pointer, host unsafe.Pointer, t *uintptr) int32
	ovSetTensor      func(req uintptr, name string, t uintptr) int32
	ovTensorFree     func(t uintptr)
	ovInfer          func(req uintptr) int32
	ovOutputByIndex  func(req uintptr, idx uintptr, t *uintptr) int32
	ovTensorSize     func(t uintptr, n *uintptr) int32
	ovTensorData     func(t uintptr, data *unsafe.Pointer) int32
	ovErrorInfo      func(status int32) string
	ovLastErr        func() string
	ovGetVersion     func(v unsafe.Pointer) int32
	ovVersionFree    func(v unsafe.Pointer)
)

// loadRuntime loads OpenVINO's libraries from RuntimeDir by absolute path, once.
func loadRuntime() error {
	loadOnce.Do(func() {
		for _, name := range []string{"libtbb.so.12", "libopenvino.so." + runtimeSoname} {
			if _, err := purego.Dlopen(RuntimeDir+"/"+name, purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
				loadErr = fmt.Errorf("loading OpenVINO from %s: %w", RuntimeDir, err)
				return
			}
		}
		lib, err := purego.Dlopen(RuntimeDir+"/libopenvino_c.so."+runtimeSoname, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("loading OpenVINO from %s: %w", RuntimeDir, err)
			return
		}
		for name, fn := range map[string]any{
			"ov_core_create": &ovCoreCreate, "ov_core_free": &ovCoreFree, "ov_core_read_model": &ovReadModel,
			"ov_core_compile_model_props": &ovCompileProps, "ov_model_free": &ovModelFree,
			"ov_compiled_model_free": &ovCompiledFree, "ov_compiled_model_create_infer_request": &ovCreateRequest,
			"ov_infer_request_free": &ovRequestFree, "ov_tensor_create_from_host_ptr": &ovTensorFromHost,
			"ov_infer_request_set_tensor": &ovSetTensor, "ov_tensor_free": &ovTensorFree,
			"ov_infer_request_infer": &ovInfer, "ov_infer_request_get_output_tensor_by_index": &ovOutputByIndex, "ov_tensor_get_size": &ovTensorSize,
			"ov_tensor_data": &ovTensorData, "ov_get_error_info": &ovErrorInfo, "ov_get_last_err_msg": &ovLastErr,
			"ov_get_openvino_version": &ovGetVersion, "ov_version_free": &ovVersionFree,
		} {
			purego.RegisterLibFunc(fn, lib, name)
		}
	})
	return loadErr
}

func ovStatus(s int32, what string) error {
	if s == 0 {
		return nil
	}
	return fmt.Errorf("openvino %s: %s: %s", what, ovErrorInfo(s), ovLastErr())
}

// runtimeVersion is the loaded runtime's build number, such as "2026.4.1-22982-...".
func runtimeVersion() (string, error) {
	var v struct{ build, desc *byte }
	if err := ovStatus(ovGetVersion(unsafe.Pointer(&v)), "version"); err != nil {
		return "", err
	}
	defer ovVersionFree(unsafe.Pointer(&v))
	var b []byte
	for p := unsafe.Pointer(v.build); *(*byte)(p) != 0; p = unsafe.Add(p, 1) {
		b = append(b, *(*byte)(p))
	}
	return string(b), nil
}

type ovCore struct{ c uintptr }

func newCore() (*ovCore, error) {
	if err := loadRuntime(); err != nil {
		return nil, err
	}
	var c uintptr
	if err := ovStatus(ovCoreCreate(&c), "core"); err != nil {
		return nil, err
	}
	return &ovCore{c}, nil
}

func (c *ovCore) close() { ovCoreFree(c.c) }

func cstr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

// graph is one compiled graph with its one infer request, used by one call at a time.
type graph struct {
	name     string
	inputs   []string
	compiled uintptr
	req      uintptr
	pin      runtime.Pinner
	bufs     [][]byte
}

// load reads and compiles an IR graph for the CPU with the given properties.
func (c *ovCore) load(name, xml string, inputs []string, props map[string]string) (*graph, error) {
	var m uintptr
	if err := ovStatus(ovReadModel(c.c, xml, 0, &m), "read "+xml); err != nil {
		return nil, err
	}
	defer ovModelFree(m)
	kv := make([][2]*byte, 0, len(props))
	var p runtime.Pinner
	defer p.Unpin()
	for k, v := range props {
		pair := [2]*byte{cstr(k), cstr(v)}
		p.Pin(pair[0])
		p.Pin(pair[1])
		kv = append(kv, pair)
	}
	var argp unsafe.Pointer
	if len(kv) > 0 {
		argp = unsafe.Pointer(&kv[0])
	}
	g := &graph{name: name, inputs: inputs}
	if err := ovStatus(ovCompileProps(c.c, m, "CPU", uintptr(len(kv)), argp, &g.compiled), "compile "+xml); err != nil {
		return nil, err
	}
	runtime.KeepAlive(kv)
	if err := ovStatus(ovCreateRequest(g.compiled, &g.req), "infer request"); err != nil {
		ovCompiledFree(g.compiled)
		return nil, err
	}
	g.bufs = make([][]byte, len(inputs))
	return g, nil
}

func (g *graph) close() {
	ovRequestFree(g.req)
	ovCompiledFree(g.compiled)
	g.pin.Unpin()
}

// run sets the inputs (in the order of g.inputs), runs the graph to its end and returns output 0. A call is never
// cancelled: after ov_infer_request_cancel, later calls of the graph fail OpenVINO's shape inference. The input buffers
// are Go memory pinned for the graph's life: the request keeps their address.
func (g *graph) run(in []tensor) ([]float32, error) {
	for i, x := range in {
		n, typ := len(x.i64)*8, int32(elemI64)
		if x.f32 != nil {
			n, typ = len(x.f32)*4, elemF32
		}
		if len(g.bufs[i]) < n {
			g.bufs[i] = make([]byte, n)
			g.pin.Pin(&g.bufs[i][0])
		}
		buf := unsafe.Pointer(&g.bufs[i][0])
		if typ == elemF32 {
			copy(unsafe.Slice((*float32)(buf), len(x.f32)), x.f32)
		} else {
			copy(unsafe.Slice((*int64)(buf), len(x.i64)), x.i64)
		}
		dims := append([]int64(nil), x.shape...)
		var t uintptr
		// ov_shape_t {int64 rank; int64* dims} by value is two integer registers: rank, dims.
		err := ovStatus(ovTensorFromHost(typ, int64(len(dims)), unsafe.Pointer(&dims[0]), buf, &t), "tensor")
		runtime.KeepAlive(dims)
		if err != nil {
			return nil, err
		}
		err = ovStatus(ovSetTensor(g.req, g.inputs[i], t), "set "+g.inputs[i])
		ovTensorFree(t)
		if err != nil {
			return nil, err
		}
	}
	if err := ovStatus(ovInfer(g.req), "infer "+g.name); err != nil {
		return nil, err
	}
	var t uintptr
	if err := ovStatus(ovOutputByIndex(g.req, 0, &t), "output"); err != nil {
		return nil, err
	}
	defer ovTensorFree(t)
	var n uintptr
	var data unsafe.Pointer
	if err := ovStatus(ovTensorSize(t, &n), "size"); err != nil {
		return nil, err
	}
	if err := ovStatus(ovTensorData(t, &data), "data"); err != nil {
		return nil, err
	}
	return append([]float32(nil), unsafe.Slice((*float32)(data), int(n))...), nil
}
