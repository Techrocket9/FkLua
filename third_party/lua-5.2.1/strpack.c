/*
** strpack.c -- string.pack / string.unpack / string.packsize for Lua 5.2.1.
**
** Factorio's Lua is 5.2.1 with these three functions grafted in from 5.4.6.
** Stock 5.2.1 has no such thing, so without this file bin/lua52f is missing a
** feature the game has -- and an oracle that lacks something the real sandbox
** provides is worse than useless for the code that depends on it.
**
** Derived from Lua 5.4.6's lstrlib.c (MIT, Copyright 1994-2023 Lua.org,
** PUC-Rio). Kept as an ordinary C file rather than a patch hunk so it can be
** read and diffed against upstream; patches/02-string-pack.patch only adds the
** #include and three luaL_Reg entries.
**
** ------------------------------------------------------------------------
** THE ONE REAL DIFFERENCE FROM 5.4, AND WHY
**
** Lua 5.4 has an integer subtype: an integer option reads its argument with
** lua_tointegerx and RAISES "number has no integer representation" for 3.5.
** Factorio's 5.2.1 has only doubles, and the in-game probe measured what it
** actually does (testdata/probe/results/probe.json, and the table in
** agents/sandbox.md):
**
**     string.pack("<I4", 3.5)          packs 3        -- truncates, no error
**     string.pack("<I4", 4000000000)   round-trips    -- u32 is safe
**     string.pack("<I8", 2^53+1)       comes back 2^53
**     string.pack("<i8", -1)           round-trips -1
**
** So getinteger below reads a lua_Number and CASTS, which truncates toward
** zero, and results are pushed back as lua_Number. Matching the game's
** observed behaviour is the whole point of this file; matching upstream 5.4
** would reintroduce the drift it exists to remove.
*/

#include <limits.h>
#include <stddef.h>
#include <string.h>

/*
** The integer type used to carry a packed value. 5.2.1's lua_Integer is
** ptrdiff_t, which is 64-bit where we build but is not guaranteed to be, and
** the formats explicitly speak in widths. long long is exact about it.
*/
typedef long long fk_Int;
typedef unsigned long long fk_UInt;

#define FK_MAXINTSIZE 16
#define FK_NB CHAR_BIT
#define FK_MC ((1 << FK_NB) - 1)
#define FK_SZINT ((int)sizeof(fk_Int))
#define FK_PADBYTE 0x00

/* Native endianness, decided at run time so cross-compiling cannot get it
** wrong the way a #ifdef ladder can. */
static const union {
  int dummy;
  char little;
} fk_nativeendian = {1};

struct fk_cD {
  char c;
  union { double d; void *p; long l; } u;
};

#define FK_MAXALIGN (offsetof(struct fk_cD, u))

typedef union fk_Ftypes {
  float f;
  double d;
  char buff[5 * sizeof(double)];
} fk_Ftypes;

typedef struct fk_Header {
  lua_State *L;
  int islittle;
  int maxalign;
} fk_Header;

typedef enum fk_KOption {
  fk_Kint,        /* signed integer */
  fk_Kuint,       /* unsigned integer */
  fk_Kfloat,      /* single-precision float */
  fk_Kdouble,     /* double-precision float */
  fk_Kchar,       /* fixed-length string */
  fk_Kstring,     /* string preceded by its length */
  fk_Kzstr,       /* zero-terminated string */
  fk_Kpadding,    /* padding */
  fk_Kpaddalign,  /* padding for alignment */
  fk_Knop         /* no-op (configuration item) */
} fk_KOption;

/* Read an argument as an integer, TRUNCATING rather than raising -- see the
** header comment. */
static fk_Int fk_getinteger (lua_State *L, int arg) {
  return (fk_Int)luaL_checknumber(L, arg);
}

static int fk_digit (int c) { return '0' <= c && c <= '9'; }

static int fk_getnum (const char **fmt, int df) {
  if (!fk_digit(**fmt))
    return df;
  else {
    int a = 0;
    do {
      a = a * 10 + (*((*fmt)++) - '0');
    } while (fk_digit(**fmt) && a <= ((int)INT_MAX - 9) / 10);
    return a;
  }
}

static int fk_getnumlimit (fk_Header *h, const char **fmt, int df) {
  int sz = fk_getnum(fmt, df);
  if (sz > FK_MAXINTSIZE || sz <= 0)
    return luaL_error(h->L, "integral size (%d) out of limits [1,%d]",
                      sz, FK_MAXINTSIZE);
  return sz;
}

static void fk_initheader (lua_State *L, fk_Header *h) {
  h->L = L;
  h->islittle = fk_nativeendian.little;
  h->maxalign = 1;
}

static fk_KOption fk_getoption (fk_Header *h, const char **fmt, int *size) {
  int opt = *((*fmt)++);
  *size = 0;
  switch (opt) {
    case 'b': *size = sizeof(char); return fk_Kint;
    case 'B': *size = sizeof(char); return fk_Kuint;
    case 'h': *size = sizeof(short); return fk_Kint;
    case 'H': *size = sizeof(short); return fk_Kuint;
    case 'l': *size = sizeof(long); return fk_Kint;
    case 'L': *size = sizeof(long); return fk_Kuint;
    case 'j': *size = FK_SZINT; return fk_Kint;
    case 'J': *size = FK_SZINT; return fk_Kuint;
    case 'T': *size = sizeof(size_t); return fk_Kuint;
    case 'f': *size = sizeof(float); return fk_Kfloat;
    case 'n': case 'd': *size = sizeof(double); return fk_Kdouble;
    case 'i': *size = fk_getnumlimit(h, fmt, sizeof(int)); return fk_Kint;
    case 'I': *size = fk_getnumlimit(h, fmt, sizeof(int)); return fk_Kuint;
    case 's': *size = fk_getnumlimit(h, fmt, sizeof(size_t)); return fk_Kstring;
    case 'c':
      *size = fk_getnum(fmt, -1);
      if (*size == -1)
        luaL_error(h->L, "missing size for format option 'c'");
      return fk_Kchar;
    case 'z': return fk_Kzstr;
    case 'x': *size = 1; return fk_Kpadding;
    case 'X': return fk_Kpaddalign;
    case ' ': break;
    case '<': h->islittle = 1; break;
    case '>': h->islittle = 0; break;
    case '=': h->islittle = fk_nativeendian.little; break;
    case '!': h->maxalign = fk_getnumlimit(h, fmt, (int)FK_MAXALIGN); break;
    default: luaL_error(h->L, "invalid format option '%c'", opt);
  }
  return fk_Knop;
}

static fk_KOption fk_getdetails (fk_Header *h, size_t totalsize,
                                 const char **fmt, int *psize, int *ntoalign) {
  fk_KOption opt = fk_getoption(h, fmt, psize);
  int align = *psize;
  if (opt == fk_Kpaddalign) {
    if (**fmt == '\0' || fk_getoption(h, fmt, &align) == fk_Kchar || align == 0)
      luaL_argerror(h->L, 1, "invalid next option for option 'X'");
  }
  if (align <= 1 || opt == fk_Kchar)
    *ntoalign = 0;
  else {
    if (align > h->maxalign)
      align = h->maxalign;
    if ((align & (align - 1)) != 0)
      luaL_argerror(h->L, 1, "format asks for alignment not power of 2");
    *ntoalign = (align - (int)(totalsize & (align - 1))) & (align - 1);
  }
  return opt;
}

/* Pack n into buff, respecting endianness. Sign-extends when the value is
** negative and the field is wider than the value. */
static void fk_packint (luaL_Buffer *b, fk_UInt n,
                        int islittle, int size, int neg) {
  char buff[FK_MAXINTSIZE];
  int i;
  buff[islittle ? 0 : size - 1] = (char)(n & FK_MC);
  for (i = 1; i < size; i++) {
    n >>= FK_NB;
    buff[islittle ? i : size - 1 - i] = (char)(n & FK_MC);
  }
  if (neg && size > FK_SZINT) {
    for (i = FK_SZINT; i < size; i++)
      buff[islittle ? i : size - 1 - i] = (char)FK_MC;
  }
  luaL_addlstring(b, buff, size);
}

static void fk_copywithendian (char *dest, const char *src,
                               int size, int islittle) {
  if (islittle == fk_nativeendian.little)
    memcpy(dest, src, size);
  else {
    int i;
    for (i = 0; i < size; i++)
      dest[i] = src[size - 1 - i];
  }
}

static int fk_str_pack (lua_State *L) {
  luaL_Buffer b;
  fk_Header h;
  const char *fmt = luaL_checkstring(L, 1);
  int arg = 1;
  size_t totalsize = 0;
  fk_initheader(L, &h);
  lua_pushnil(L);  /* placeholder, mirroring 5.4 */
  luaL_buffinit(L, &b);
  while (*fmt != '\0') {
    int size, ntoalign;
    fk_KOption opt = fk_getdetails(&h, totalsize, &fmt, &size, &ntoalign);
    totalsize += ntoalign + size;
    while (ntoalign-- > 0)
      luaL_addchar(&b, FK_PADBYTE);
    arg++;
    switch (opt) {
      case fk_Kint: {
        fk_Int n = fk_getinteger(L, arg);
        if (size < FK_SZINT) {
          fk_Int lim = (fk_Int)1 << ((size * FK_NB) - 1);
          luaL_argcheck(L, -lim <= n && n < lim, arg, "integer overflow");
        }
        fk_packint(&b, (fk_UInt)n, h.islittle, size, (n < 0));
        break;
      }
      case fk_Kuint: {
        fk_Int n = fk_getinteger(L, arg);
        if (size < FK_SZINT)
          luaL_argcheck(L, (fk_UInt)n < ((fk_UInt)1 << (size * FK_NB)),
                        arg, "unsigned overflow");
        fk_packint(&b, (fk_UInt)n, h.islittle, size, 0);
        break;
      }
      case fk_Kfloat: {
        fk_Ftypes u;
        char buff[sizeof(u)];
        u.f = (float)luaL_checknumber(L, arg);
        fk_copywithendian(buff, u.buff, size, h.islittle);
        luaL_addlstring(&b, buff, size);
        break;
      }
      case fk_Kdouble: {
        fk_Ftypes u;
        char buff[sizeof(u)];
        u.d = (double)luaL_checknumber(L, arg);
        fk_copywithendian(buff, u.buff, size, h.islittle);
        luaL_addlstring(&b, buff, size);
        break;
      }
      case fk_Kchar: {
        size_t len;
        const char *s = luaL_checklstring(L, arg, &len);
        luaL_argcheck(L, len <= (size_t)size, arg, "string longer than given size");
        luaL_addlstring(&b, s, len);
        while (len++ < (size_t)size)
          luaL_addchar(&b, FK_PADBYTE);
        break;
      }
      case fk_Kstring: {
        size_t len;
        const char *s = luaL_checklstring(L, arg, &len);
        luaL_argcheck(L, size >= (int)sizeof(size_t) || len < ((size_t)1 << (size * FK_NB)),
                      arg, "string length does not fit in given size");
        fk_packint(&b, (fk_UInt)len, h.islittle, size, 0);
        luaL_addlstring(&b, s, len);
        totalsize += len;
        break;
      }
      case fk_Kzstr: {
        size_t len;
        const char *s = luaL_checklstring(L, arg, &len);
        luaL_argcheck(L, strlen(s) == len, arg, "string contains zeros");
        luaL_addlstring(&b, s, len);
        luaL_addchar(&b, '\0');
        totalsize += len + 1;
        break;
      }
      case fk_Kpadding: luaL_addchar(&b, FK_PADBYTE); /* fall through */
      case fk_Kpaddalign: case fk_Knop:
        arg--;
        break;
    }
  }
  luaL_pushresult(&b);
  return 1;
}

static int fk_str_packsize (lua_State *L) {
  fk_Header h;
  const char *fmt = luaL_checkstring(L, 1);
  size_t totalsize = 0;
  fk_initheader(L, &h);
  while (*fmt != '\0') {
    int size, ntoalign;
    fk_KOption opt = fk_getdetails(&h, totalsize, &fmt, &size, &ntoalign);
    if (opt == fk_Kstring || opt == fk_Kzstr)
      return luaL_error(L, "variable-size format in packsize");
    size += ntoalign;
    totalsize += size;
  }
  lua_pushnumber(L, (lua_Number)totalsize);
  return 1;
}

static fk_Int fk_unpackint (lua_State *L, const char *str, int islittle,
                            int size, int issigned) {
  fk_UInt res = 0;
  int limit = (size <= FK_SZINT) ? size : FK_SZINT;
  int i;
  for (i = limit - 1; i >= 0; i--) {
    res <<= FK_NB;
    res |= (fk_UInt)(unsigned char)str[islittle ? i : size - 1 - i];
  }
  if (size < FK_SZINT) {
    if (issigned) {
      fk_UInt mask = (fk_UInt)1 << (size * FK_NB - 1);
      res = ((res ^ mask) - mask);
    }
  }
  else if (size > FK_SZINT) {
    int mask = (!issigned || (fk_Int)res >= 0) ? 0 : FK_MC;
    for (i = limit; i < size; i++) {
      if ((unsigned char)str[islittle ? i : size - 1 - i] != mask)
        luaL_error(L, "%d-byte integer does not fit into Lua Integer", size);
    }
  }
  return (fk_Int)res;
}

static int fk_str_unpack (lua_State *L) {
  fk_Header h;
  const char *fmt = luaL_checkstring(L, 1);
  size_t ld;
  const char *data = luaL_checklstring(L, 2, &ld);
  size_t pos = (size_t)luaL_optnumber(L, 3, 1) - 1;
  int n = 0;
  luaL_argcheck(L, pos <= ld, 3, "initial position out of string");
  fk_initheader(L, &h);
  while (*fmt != '\0') {
    int size, ntoalign;
    fk_KOption opt = fk_getdetails(&h, pos, &fmt, &size, &ntoalign);
    luaL_argcheck(L, (size_t)ntoalign + size <= ld - pos, 2,
                  "data string too short");
    pos += ntoalign;
    luaL_checkstack(L, 2, "too many results");
    n++;
    switch (opt) {
      case fk_Kint:
      case fk_Kuint: {
        fk_Int res = fk_unpackint(L, data + pos, h.islittle, size,
                                  (opt == fk_Kint));
        /* An unsigned field as wide as fk_Int comes back through a signed
        ** type; reinterpret before widening to double, or 0xFFFFFFFFFFFFFFFF
        ** would arrive as -1. */
        if (opt == fk_Kuint && size >= FK_SZINT)
          lua_pushnumber(L, (lua_Number)(fk_UInt)res);
        else
          lua_pushnumber(L, (lua_Number)res);
        break;
      }
      case fk_Kfloat: {
        fk_Ftypes u;
        fk_copywithendian(u.buff, data + pos, size, h.islittle);
        lua_pushnumber(L, (lua_Number)u.f);
        break;
      }
      case fk_Kdouble: {
        fk_Ftypes u;
        fk_copywithendian(u.buff, data + pos, size, h.islittle);
        lua_pushnumber(L, (lua_Number)u.d);
        break;
      }
      case fk_Kchar: {
        lua_pushlstring(L, data + pos, size);
        break;
      }
      case fk_Kstring: {
        size_t len = (size_t)fk_unpackint(L, data + pos, h.islittle, size, 0);
        luaL_argcheck(L, len <= ld - pos - size, 2, "data string too short");
        lua_pushlstring(L, data + pos + size, len);
        pos += len;
        break;
      }
      case fk_Kzstr: {
        size_t len = strlen(data + pos);
        luaL_argcheck(L, pos + len < ld, 2, "unfinished string for format 'z'");
        lua_pushlstring(L, data + pos, len);
        pos += len + 1;
        break;
      }
      case fk_Kpaddalign: case fk_Kpadding: case fk_Knop:
        n--;
        break;
    }
    pos += size;
  }
  lua_pushnumber(L, (lua_Number)(pos + 1));  /* next position */
  return n + 1;
}
