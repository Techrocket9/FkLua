;; Kernel: integer division by a compile-time constant.
;;
;; Until the constant-divisor lowering, `i32.div_u` and `i32.rem_u` were an
;; UNCONDITIONAL helper call at every level -- and the call is the expensive
;; part. Breaking one i32 load into its pieces measured the CALL at 34% of its
;; cost, the largest single component, and a division helper is the same shape:
;; a zero check and two arithmetic operations behind a function call.
;;
;; The two shapes here are the ones a real guest actually contains, taken from
;; the TinyGo bench guest's own wasm rather than imagined:
;;
;;   - INDEX DECOMPOSITION. `n % stride` and `n / stride` on a non-power-of-two
;;     stride, which is what a flood fill does per cell to recover (x, y). Note
;;     that the bench guest's real_grid does NOT hit this path -- its `side` is
;;     a parameter of an exported function, so LLVM cannot fold it. Written by
;;     hand for the same reason count.wat and frame.wat are: the shape is real,
;;     the specific guest that would show it is not the one in bench/guests.
;;
;;   - DECIMAL DIGIT EXTRACTION. `/ 10` and the `- q*10` that recovers the
;;     remainder from it, which is exactly what LLVM emits for itoa and what
;;     real_names runs once per digit. That one IS in the bench guest.
;;
;; A power-of-two divisor is included as a control: it lowers to the same
;; expression a constant `shr_u` already did, so it should not move, and a
;; number that says otherwise is measuring the harness.
(module
  (func (export "kernel") (param $n i32) (result i32)
    (local $i i32) (local $acc i32) (local $q i32)
    (block $done
      (br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
      (loop $top
        ;; (x, y) from a linear index, stride 337 -- prime, so nothing here
        ;; can quietly become a shift.
        (local.set $acc
          (i32.add (local.get $acc)
            (i32.add (i32.rem_u (local.get $i) (i32.const 337))
                     (i32.div_u (local.get $i) (i32.const 337)))))

        ;; itoa's inner step: one quotient, and the remainder recovered from it.
        (local.set $q (i32.div_u (local.get $acc) (i32.const 10)))
        (local.set $acc
          (i32.add (local.get $q)
            (i32.sub (local.get $acc)
                     (i32.mul (local.get $q) (i32.const 10)))))

        ;; The control: a power of two, which was already cheap.
        (local.set $acc
          (i32.add (local.get $acc)
                   (i32.div_u (local.get $i) (i32.const 64))))

        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
    (local.get $acc))
)
