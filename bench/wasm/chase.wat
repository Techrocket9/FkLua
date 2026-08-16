;; Kernel: pointer chase over {next: u32, val: u32} nodes in linear memory.
;;
;; The list is built in a shuffled order, as in bench/kernels/chase.lua, so the
;; access pattern is not sequential and the flat word table gets no unfair help
;; from locality.
(module
  (memory (export "memory") 4)

  (func $order (param $i i32) (param $n i32) (result i32)
    (i32.rem_u (i32.mul (local.get $i) (i32.const 40503)) (local.get $n)))

  (func (export "setup") (param $n i32)
    (local $k i32) (local $this i32) (local $next i32)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $k) (local.get $n)))
        (local.set $this (call $order (local.get $k) (local.get $n)))
        (local.set $next (call $order
          (i32.rem_u (i32.add (local.get $k) (i32.const 1)) (local.get $n))
          (local.get $n)))
        (i32.store (i32.mul (local.get $this) (i32.const 8))
                   (i32.mul (local.get $next) (i32.const 8)))
        (i32.store (i32.add (i32.mul (local.get $this) (i32.const 8)) (i32.const 4))
                   (i32.mul (local.get $this) (i32.const 2654435761)))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (br $top))))

  (func (export "kernel") (param $p i32) (param $n i32) (result i32)
    (local $i i32) (local $s i32)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $s (i32.add (local.get $s)
          (i32.load offset=4 (local.get $p))))
        (local.set $p (i32.load (local.get $p)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top)))
    (local.get $s))
)
