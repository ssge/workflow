# R1 forms

`Parse(source, compiler)` validates a schema-19 form and compiles every dynamic
text expression. A `Compiler` receives FEEL source without the leading `=` and
returns a pure `Evaluator` or a compilation error. Static-only forms can pass a
nil compiler. The FEEL package and its adapter are outside this package.

The resulting `Template` has `ID()`, `Render(variables)` and
`ValidateSubmission(fields)`. Render returns a fresh JSON-ready `View`; it
never applies display defaults to variables. The supplied evaluator must not
mutate its variables. Submission validation returns only supplied writes, or
field errors with no writes. Callers apply writes only after successful
validation and own task persistence and transaction boundaries.

`*Error` carries an R1 code, component ID, field and reason. Callers add the
logical source filename when reporting deployment diagnostics. Render errors
are activation failures; submission errors use `FieldError` instead.
