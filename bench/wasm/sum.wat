;; Kernel: sum a u32 array out of linear memory.
;;
;; The wasm counterpart of bench/kernels/sum.lua's "gen" variant, compiled by
;; FkLua rather than hand-written to look like its output. That is the whole
;; point: the M0 kernels pin the ceiling, this one measures what the compiler
;; actually reaches.
(module
  (memory (export "memory") 4)

  ;; Fill n words with a hash so the sum depends on every one of them.
  (func (export "setup") (param $n i32)
    (local $i i32)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (i32.store (i32.mul (local.get $i) (i32.const 4))
                   (i32.mul (local.get $i) (i32.const 2654435761)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top))))

  (func (export "kernel") (param $p i32) (param $n i32) (result i32)
    (local $i i32) (local $s i32)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $s (i32.add (local.get $s)
          (i32.load (i32.add (local.get $p) (i32.mul (local.get $i) (i32.const 4))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top)))
    (local.get $s))
)
