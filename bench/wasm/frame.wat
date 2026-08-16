;; Kernel: f64 accumulation through a shadow-stack frame that never escapes.
;;
;; This is the shape typed-slot promotion targets: LLVM spilling an f64 to the
;; shadow stack because the source language took its address, with no pointer to
;; it ever leaving the function. Every iteration is an f64 store and an f64
;; load, which without promotion is an IEEE-754 disassembly and reassembly
;; through two u32 words.
;;
;; Written by hand rather than produced by TinyGo on purpose -- see
;; agents/codegen.md: TinyGo at -opt=z has already promoted everything that does
;; not escape, so its remaining frames all escape and none of them qualify.
(module
  (memory (export "memory") 1)
  (global $sp (mut i32) (i32.const 65536))

  (func (export "kernel") (param $n i32) (result f64)
    (local $fp i32) (local $i i32)
    (global.set $sp (local.tee $fp (i32.sub (global.get $sp) (i32.const 16))))
    (f64.store offset=8 (local.get $fp) (f64.const 0))
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (f64.store (local.get $fp)
          (f64.mul (f64.convert_i32_u (local.get $i)) (f64.const 0.5)))
        (f64.store offset=8 (local.get $fp)
          (f64.add (f64.load offset=8 (local.get $fp))
                   (f64.load (local.get $fp))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top)))
    (global.set $sp (i32.add (local.get $fp) (i32.const 16)))
    (f64.load offset=8 (local.get $fp)))
)
