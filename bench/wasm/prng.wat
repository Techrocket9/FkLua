;; Kernel: xorshift32. Shifts, xor and 32-bit wrapping, and nothing else.
(module
  (func (export "kernel") (param $n i32) (result i32)
    (local $i i32) (local $x i32)
    (local.set $x (i32.const 2463534242))
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $x (i32.xor (local.get $x) (i32.shl (local.get $x) (i32.const 13))))
        (local.set $x (i32.xor (local.get $x) (i32.shr_u (local.get $x) (i32.const 17))))
        (local.set $x (i32.xor (local.get $x) (i32.shl (local.get $x) (i32.const 5))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top)))
    (local.get $x))
)
