;; Kernel: f64 dot product out of linear memory.
;;
;; The 11.18x kernel from M0. Every element costs an ld_f64, which reassembles
;; an IEEE-754 double from two u32 words -- the cost typed-slot promotion exists
;; to avoid, and cannot here, because these arrays are heap-resident and their
;; addresses are function parameters.
(module
  (memory (export "memory") 4)

  (func (export "setup") (param $n i32)
    (local $i i32)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (f64.store (i32.mul (local.get $i) (i32.const 8))
          (f64.add (f64.const 1.0)
            (f64.mul (f64.convert_i32_u (local.get $i)) (f64.const 0.000030517578125))))
        (f64.store (i32.add (i32.mul (local.get $n) (i32.const 8))
                            (i32.mul (local.get $i) (i32.const 8)))
          (f64.sub (f64.const 2.0)
            (f64.mul (f64.convert_i32_u (local.get $i)) (f64.const 0.000015258789062))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top))))

  (func (export "kernel") (param $pa i32) (param $pb i32) (param $n i32) (result f64)
    (local $i i32) (local $s f64)
    (block $done
      (loop $top
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $s (f64.add (local.get $s)
          (f64.mul (f64.load (local.get $pa)) (f64.load (local.get $pb)))))
        (local.set $pa (i32.add (local.get $pa) (i32.const 8)))
        (local.set $pb (i32.add (local.get $pb) (i32.const 8)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $top)))
    (local.get $s))
)
