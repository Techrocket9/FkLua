;; Kernel: the canonical counted loop -- `for (int i = 0; i < n; i++)`.
;;
;; The one shape every compiled program is full of, and the one the M5
;; block-local range analysis could say nothing about: at the loop head the
;; counter's range was thrown away, so `i32.lt_s` fell back to biasing both
;; sides by 2^31 and the increment kept a `% 2^32` it could never need.
;;
;; Written in the shape LLVM actually emits, which is not the shape the source
;; is written in. Loop rotation turns a top-tested loop into a guarded
;; bottom-tested one, and that guard -- `if (n < 1) skip` -- is load-bearing
;; here for a reason worth stating: it is the only thing that proves the trip
;; count is a non-negative signed number. Without it `n` could be negative, the
;; unsigned and signed orders would disagree, and the compare would have to keep
;; its bias no matter how well the counter itself is known.
;;
;; Hand-written for the same reason frame.wat is: TinyGo at -opt=z strength
;; reduces most counted loops into a countdown against zero, so its output no
;; longer contains the shape a front end wrote.
(module
  (func (export "kernel") (param $n i32) (result i32)
    (local $i i32) (local $s i32)
    (block $done
      (br_if $done (i32.lt_s (local.get $n) (i32.const 1)))
      (loop $top
        (local.set $s (i32.add (local.get $s) (local.get $i)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br_if $top (i32.lt_s (local.get $i) (local.get $n)))))
    (local.get $s))
)
