/* Minimal QuickJS WASI reactor for the #843 phase-1 spike: prove a real memory
 * cap (JS_SetMemoryLimit) and a host timeout under wazero. No std/os modules. */
#include <stddef.h>
#include "quickjs.h"

static JSRuntime *rt;
static JSContext *ctx;

#define BUFCAP (4 * 1024 * 1024)
static char input_buf[BUFCAP];

__attribute__((export_name("buffer")))
char *buffer(void) { return input_buf; }

__attribute__((export_name("buffer_cap")))
int buffer_cap(void) { return BUFCAP; }

/* init a runtime+context with an optional heap limit (bytes) and stack size. */
__attribute__((export_name("init")))
int init(unsigned int mem_limit, unsigned int stack_size) {
  rt = JS_NewRuntime();
  if (!rt) return -1;
  if (mem_limit) JS_SetMemoryLimit(rt, (size_t)mem_limit);
  if (stack_size) JS_SetMaxStackSize(rt, (size_t)stack_size);
  ctx = JS_NewContext(rt);
  return ctx ? 0 : -1;
}

/* eval the first `len` bytes of the input buffer. 0 ok, 1 JS exception
 * (e.g. the memory cap tripped), -2 not initialised. */
__attribute__((export_name("eval")))
int eval(int len) {
  if (!ctx) return -2;
  JSValue v = JS_Eval(ctx, input_buf, (size_t)len, "<spike>", 0);
  int rc = JS_IsException(v) ? 1 : 0;
  JS_FreeValue(ctx, v);
  return rc;
}
