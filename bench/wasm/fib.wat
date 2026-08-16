;; Kernel: recursive fib(30). ~2.7M calls, so this is call dispatch and little
;; else -- which is what upvalue promotion moves.
(module
  (func $fib (export "kernel") (param $n i32) (result i32)
    (if (i32.lt_u (local.get $n) (i32.const 2))
      (then (return (local.get $n))))
    (i32.add
      (call $fib (i32.sub (local.get $n) (i32.const 1)))
      (call $fib (i32.sub (local.get $n) (i32.const 2)))))
)
