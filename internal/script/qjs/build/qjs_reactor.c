/* QuickJS WASI reactor for the Kora script runtime (#843). Core QuickJS only:
 * no quickjs-libc, so no WASI environ/signal gaps. The host talks to the guest
 * through one input buffer, one output buffer, and one imported call that
 * carries a JSON request and returns a JSON response. */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "quickjs.h"

/* Added to quickjs.c by build/quickjs-reseed.patch (applied by build.sh). */
void JS_KoraReseedRandom(JSContext *ctx, uint64_t seed);

__attribute__((import_module("kora_host"), import_name("call")))
extern int host_call(const char *req, int len, char *dst, int cap);
__attribute__((import_module("kora_host"), import_name("take")))
extern void host_take(char *dst);

/* Preallocated in init, so the snapshot carries both buffers and a run never grows them. */
#define IN_BUF_BYTES (64 * 1024)
#define RESP_BUF_BYTES 4096
static char *in_buf;
static int in_cap;
static char *resp_buf;
static int resp_cap;
static char *out_buf;
static int out_len;
static JSRuntime *rt;
static JSContext *ctx;

/* buffer_reserve(n) -> ptr: a heap input buffer of at least n+1 bytes (room for
 * the NUL terminator). It keeps the current buffer when that is already big
 * enough, and returns NULL when the allocation fails. */
__attribute__((export_name("buffer_reserve")))
char *buffer_reserve(int n) {
  if (n < 0) return NULL;
  if (n + 1 <= in_cap) return in_buf;
  char *p = realloc(in_buf, (size_t)n + 1);
  if (!p) return NULL;
  in_buf = p;
  in_cap = n + 1;
  return in_buf;
}
__attribute__((export_name("output"))) char *output(void) { return out_buf; }
__attribute__((export_name("output_len"))) int output_len(void) { return out_len; }

static void set_output(const char *s, size_t n) {
  free(out_buf);
  out_buf = NULL;
  out_len = 0;
  if (!s) return;
  out_buf = malloc(n ? n : 1);
  if (!out_buf) return;
  memcpy(out_buf, s, n);
  out_len = (int)n;
}

/* __kora_host(requestJSON) -> responseJSON: one synchronous host call. The host writes
 * the response into resp_buf when it fits (one crossing); a larger response is returned
 * by length only, so the buffer grows to it and host_take copies it in. */
static JSValue js_host(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  size_t n;
  const char *req = JS_ToCStringLen(c, &n, argc > 0 ? argv[0] : JS_UNDEFINED);
  if (!req) return JS_EXCEPTION;
  int rn = host_call(req, (int)n, resp_buf, resp_cap);
  JS_FreeCString(c, req);
  if (rn < 0) return JS_ThrowInternalError(c, "host call failed");
  if (rn > resp_cap) {
    char *p = realloc(resp_buf, (size_t)rn + 1);
    if (!p) return JS_ThrowInternalError(c, "host response buffer");
    resp_buf = p;
    resp_cap = rn;
    host_take(resp_buf);
  }
  return JS_NewStringLen(c, resp_buf, (size_t)rn);
}

__attribute__((export_name("init")))
int init(unsigned int mem_limit, unsigned int stack_size) {
  rt = JS_NewRuntime();
  if (!rt) return -1;
  in_buf = malloc(IN_BUF_BYTES);
  resp_buf = malloc(RESP_BUF_BYTES);
  if (!in_buf || !resp_buf) return -1;
  in_cap = IN_BUF_BYTES;
  resp_cap = RESP_BUF_BYTES;
  if (mem_limit) JS_SetMemoryLimit(rt, (size_t)mem_limit);
  if (stack_size) JS_SetMaxStackSize(rt, (size_t)stack_size);
  ctx = JS_NewContext(rt);
  if (!ctx) return -1;
  JSValue g = JS_GetGlobalObject(ctx);
  JS_SetPropertyStr(ctx, g, "__kora_host", JS_NewCFunction(ctx, js_host, "__kora_host", 1));
  JS_FreeValue(ctx, g);
  return 0;
}

/* reseed(lo, hi): a fresh Math.random sequence for a runtime restored from a snapshot. */
__attribute__((export_name("reseed")))
void reseed(unsigned int lo, unsigned int hi) {
  if (ctx) JS_KoraReseedRandom(ctx, ((uint64_t)hi << 32) | lo);
}

#define EVAL_COMPILE_ONLY 1
#define EVAL_JSON_RESULT 2

/* finish turns the value of one evaluation into the reactor's answer (see eval). It
 * consumes v: 0 with output = JSON of the value (EVAL_JSON_RESULT) or its string,
 * 1 with output = the exception message. */
static int finish(JSValue v, int flags) {
  if (!JS_IsException(v) && (flags & EVAL_JSON_RESULT)) {
    JSValue s = JS_JSONStringify(ctx, v, JS_UNDEFINED, JS_UNDEFINED);
    JS_FreeValue(ctx, v);
    v = s;
  }
  if (JS_IsException(v)) {
    JSValue exc = JS_GetException(ctx);
    size_t n;
    const char *msg = JS_ToCStringLen(ctx, &n, exc);
    set_output(msg ? msg : "uncaught exception", msg ? n : 18);
    if (msg) JS_FreeCString(ctx, msg);
    JS_FreeValue(ctx, exc);
    return 1;
  }
  set_output(NULL, 0);
  if (JS_IsString(v)) {
    size_t n;
    const char *s = JS_ToCStringLen(ctx, &n, v);
    if (s) { set_output(s, n); JS_FreeCString(ctx, s); }
  }
  JS_FreeValue(ctx, v);
  return 0;
}

/* eval the first `len` bytes of the input buffer as a global script.
 * 0 ok (output = JSON of the completion value when EVAL_JSON_RESULT, empty
 * when that value has no JSON form), 1 JS exception (output = its message),
 * -2 not initialised. */
__attribute__((export_name("eval")))
int eval(int len, int flags) {
  if (!ctx) return -2;
  int jsflags = JS_EVAL_TYPE_GLOBAL | ((flags & EVAL_COMPILE_ONLY) ? JS_EVAL_FLAG_COMPILE_ONLY : 0);
  return finish(JS_Eval(ctx, in_buf, (size_t)len, "<script>", jsflags), flags);
}

/* compile the first `len` bytes of the input buffer as a global script, without running
 * it. 0 ok (output = the bytecode, binary), 1 syntax error (output = its message),
 * -2 not initialised. The bytecode is only read back by eval_bytecode of this build. */
__attribute__((export_name("compile")))
int compile(int len) {
  if (!ctx) return -2;
  JSValue f = JS_Eval(ctx, in_buf, (size_t)len, "<script>", JS_EVAL_TYPE_GLOBAL | JS_EVAL_FLAG_COMPILE_ONLY);
  if (JS_IsException(f)) return finish(f, 0);
  size_t n = 0;
  uint8_t *bc = JS_WriteObject(ctx, &n, f, JS_WRITE_OBJ_BYTECODE);
  JS_FreeValue(ctx, f);
  if (!bc) return finish(JS_ThrowInternalError(ctx, "bytecode write failed"), 0);
  set_output((const char *)bc, n);
  js_free(ctx, bc);
  return 0;
}

/* eval_bytecode runs the bytecode in the first `len` bytes of the input buffer, as eval
 * runs source (same flags and answers). The bytecode must come from compile of this
 * build: the host only feeds it bytes it compiled itself. */
__attribute__((export_name("eval_bytecode")))
int eval_bytecode(int len, int flags) {
  if (!ctx) return -2;
  JSValue f = JS_ReadObject(ctx, (const uint8_t *)in_buf, (size_t)len, JS_READ_OBJ_BYTECODE);
  if (JS_IsException(f)) return finish(f, 0);
  return finish(JS_EvalFunction(ctx, f), flags);
}
