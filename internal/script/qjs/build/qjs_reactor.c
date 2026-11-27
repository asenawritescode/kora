/* QuickJS WASI reactor for the Kora script runtime (#843). Core QuickJS only:
 * no quickjs-libc, so no WASI environ/signal gaps. The host talks to the guest
 * through one input buffer, one output buffer, and one imported call that
 * carries a JSON request and returns a JSON response. */
#include <stdlib.h>
#include <string.h>
#include "quickjs.h"

__attribute__((import_module("kora_host"), import_name("call")))
extern int host_call(const char *req, int len);
__attribute__((import_module("kora_host"), import_name("take")))
extern void host_take(char *dst);

#define BUFCAP (4 * 1024 * 1024)
static char input_buf[BUFCAP];
static char *out_buf;
static int out_len;
static JSRuntime *rt;
static JSContext *ctx;

__attribute__((export_name("buffer"))) char *buffer(void) { return input_buf; }
__attribute__((export_name("buffer_cap"))) int buffer_cap(void) { return BUFCAP; }
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

/* __kora_host(requestJSON) -> responseJSON: one synchronous host call. */
static JSValue js_host(JSContext *c, JSValueConst this_val, int argc, JSValueConst *argv) {
  size_t n;
  const char *req = JS_ToCStringLen(c, &n, argc > 0 ? argv[0] : JS_UNDEFINED);
  if (!req) return JS_EXCEPTION;
  int rn = host_call(req, (int)n);
  JS_FreeCString(c, req);
  if (rn < 0) return JS_ThrowInternalError(c, "host call failed");
  char *resp = js_malloc(c, (size_t)rn + 1);
  if (!resp) return JS_EXCEPTION;
  host_take(resp);
  JSValue v = JS_NewStringLen(c, resp, (size_t)rn);
  js_free(c, resp);
  return v;
}

__attribute__((export_name("init")))
int init(unsigned int mem_limit, unsigned int stack_size) {
  rt = JS_NewRuntime();
  if (!rt) return -1;
  if (mem_limit) JS_SetMemoryLimit(rt, (size_t)mem_limit);
  if (stack_size) JS_SetMaxStackSize(rt, (size_t)stack_size);
  ctx = JS_NewContext(rt);
  if (!ctx) return -1;
  JSValue g = JS_GetGlobalObject(ctx);
  JS_SetPropertyStr(ctx, g, "__kora_host", JS_NewCFunction(ctx, js_host, "__kora_host", 1));
  JS_FreeValue(ctx, g);
  return 0;
}

#define EVAL_COMPILE_ONLY 1
#define EVAL_JSON_RESULT 2

/* eval the first `len` bytes of the input buffer as a global script.
 * 0 ok (output = JSON of the completion value when EVAL_JSON_RESULT, empty
 * when that value has no JSON form), 1 JS exception (output = its message),
 * -2 not initialised. */
__attribute__((export_name("eval")))
int eval(int len, int flags) {
  if (!ctx) return -2;
  int jsflags = JS_EVAL_TYPE_GLOBAL | ((flags & EVAL_COMPILE_ONLY) ? JS_EVAL_FLAG_COMPILE_ONLY : 0);
  JSValue v = JS_Eval(ctx, input_buf, (size_t)len, "<script>", jsflags);
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
