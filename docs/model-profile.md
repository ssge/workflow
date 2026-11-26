# Executable model profile

**R1 target:** import the [BPMN process](../acceptance/release1/flow-form.bpmn),
[DMN decision](../acceptance/release1/looping_decision.dmn) and
[Camunda form](../acceptance/release1/form-textfield.form) **unchanged**.
This is a whitelist of execution behavior, not a claim of full standards
conformance. Later additions have explicit release gates in [delivery](delivery.md).

## Compile and bind

1. Accept UTF-8 `.bpmn`, `.dmn`, `.form` sources by logical filename, not paths
   on the server. Exactly one BPMN file with one executable process; zero or
   more decisions/forms. The optional `.process-application` containing an
   empty JSON object is packaging metadata, not a fourth executable resource.
   Reject duplicate filenames, malformed XML/JSON and duplicate JSON keys.
   Do not resolve external entities, DTDs, includes, URLs or imports.
2. Match XML by namespace URI and local name, **not** the spelling of a prefix.
   R1 reads BPMN `http://www.omg.org/spec/BPMN/20100524/MODEL`, DMN
   `https://www.omg.org/spec/DMN/20191111/MODEL/`, Zeebe
   `http://camunda.org/schema/zeebe/1.0`, and form `schemaVersion: 19`.
   Require unique element IDs within each XML document, unique decision IDs
   and form IDs in the bundle, unique bindable form keys within each form.
3. Build ID maps, graph adjacency, expression trees, ordered decision rules
   and form templates. Validate source/target refs, boundary attachments,
   defaults, resource ID references, graph constraints and expression syntax
   **before publication**. If BPMN `incoming`/`outgoing` lists are supplied,
   check them against the actual sequence flows. Permit cyclic process flow
   (the fixture loops); require ordinary nodes to be reachable from the start
   and to have a path to an end, counting boundary exits from their host.
4. Preserve original sources, filenames and display metadata. Ignore only
   non-executing material: diagram interchange/geometry, documentation, labels,
   exporter/modeler/layout annotations and empty extension containers (including
   `zeebe:properties`). Do not mistake diagram shape IDs for runtime element
   IDs. Unknown execution elements, attributes changing execution, binding
   modes, extension handlers, form logic or expression dialects cause
   `UNSUPPORTED_FEATURE`; never turn them into no-ops. An unresolved reference,
   invalid shape or ambiguous graph is `INVALID_MODEL`. Report the source
   location as specified in [interfaces](interfaces.md).
5. Resolve form and decision references **within this deployment**. Store an
   immutable, profile-versioned plan only after all checks pass. Starting or
   restoring an instance does not recompile against a newer bundle.

## BPMN: token rules

Unless stated otherwise, outgoing means a single unconditional sequence flow.
Multiple incoming flows on an ordinary activity are an **uncontrolled merge**:
each arriving token activates it; it does not wait for the other incoming
flows. This matters at *both* the user task and the increment script in the
fixture.

| Element accepted in R1 | Required behavior |
| --- | --- |
| One none start; none end(s) | Start has no incoming, one outgoing. Optional `zeebe:ioMapping/zeebe:output` entries `(source,target)` evaluate in declaration order; each result writes one root name visible to later entries. All entries or none commit. End has no outgoing and consumes the arriving token. |
| `sequenceFlow` | Connect nodes of this process. Conditions only on non-default outgoing flows of an exclusive split. No sequence flow enters a boundary event. |
| `userTask` | Require `zeebe:userTask` and `zeebe:formDefinition formId="..."`; no assignment semantics in R1. Create one wait, a new `taskId` and a pinned form view on each visit. Normal completion takes the task's outgoing flow. |
| `scriptTask` | Require one `zeebe:script` with FEEL `expression` and root `resultVariable`. Evaluate once per committed activation; replace that variable with the result. No scripts in host languages. |
| `businessRuleTask` | Require one `zeebe:calledDecision decisionId="..." resultVariable="..."`. Evaluate the pinned table against current variables; store its **scalar**, not an object wrapping the output column. |
| `exclusiveGateway` | Merge passes each arrival independently. For a split, evaluate non-default flow predicates in the XML document order of the `sequenceFlow` elements; take the first `true`. If none, take its unconditional `default`; if absent, incident `NO_ROUTE`. A non-Boolean predicate is `EVALUATION_ERROR`, not `false`. |
| `parallelGateway` split | One incoming and at least two unconditional outgoing flows. Consume incoming token; create one fork occurrence and one branch token per outgoing in one commit. |
| `parallelGateway` join | At least two incoming and one unconditional outgoing. Record arrival for this fork occurrence and incoming flow; park until every expected incoming has arrived. Consume the group and release **one** successor atomically. Never count arrivals from another visit or instance. |
| Interrupting `boundaryEvent` with `timerEventDefinition/timeDuration` | One per user-task host in R1. `cancelActivity` omitted or `true`. Accept a positive fixed whole-second literal `PT<n>S` and the fixture spelling `PT<n>s` (1..2,147,483,647 seconds). Persist `dueAt = enteredAt + duration`. No early firing, no repeating wake-up after a winner. Boundary outgoing and normal outgoing are independent even when they target the same node. |

**Structured parallel subset.** A split must have a unique matching join:
follow each outgoing branch to the first parallel gateway; it must be the
same join for every path, with one distinct incoming flow per branch. Branch
regions must be disjoint single-entry regions with no cycles, nesting, early
ends, cross-branch flows or exits bypassing that join. Count timer exits as
paths when checking the region. Reject unpaired/mixed split-join gateways.
Loops **around** a complete region are valid; allocate a fresh fork ID on
every visit. This permits simultaneously open tasks without claiming general
unstructured BPMN synchronization.

Examples rejected in R1: service jobs, message catches/starts, subprocesses,
multi-instance markers, inclusive gateways, non-interrupting/cyclic/date
timers, populated extension properties and execution listeners. R2b and R4
add particular constructs; they do not relax the default rejection rule.

## FEEL and data

Use a pure expression evaluator shared by mappings, scripts, gateways, form
text and DMN. BPMN expression attributes/elements and dynamic form strings
start with `=` (trim surrounding whitespace); remove exactly that leading
marker. DMN cell/input text is FEEL **without** that marker. No `eval`, SQL,
system clock, random values, network or implicit string interpolation.

* Variables are one JSON object per instance. R1 expressions read/write root
  identifiers matching `[A-Za-z_][A-Za-z0-9_]*` (not reserved literals
  `true`, `false`, `null`). An absent identifier reads as `null`; absent and
  explicit JSON null are distinguishable in the stored object. Objects and
  arrays can be stored/passed through by name, but no path assignment in R1.
* Grammar, tight to loose: literals/identifiers/`(expr)`/`string(expr)`;
  left-associative `+`; then at most one `=`, `<`, `<=`, `>` or `>=` per
  unparenthesized comparison. Literals: JSON-style quoted strings, decimals
  (optional minus), `true`, `false`, `null`. Whitespace outside strings is
  insignificant. No implicit variable declaration/type check at deploy time.
* Use finite decimal arithmetic, not binary floating-point rounding. Support
  34 significant decimal digits and adjusted exponents -6143..6144 (zero
  allowed); strip insignificant trailing zeros before checking. Reject
  out-of-range inputs; an unrepresentable computed result is an evaluation
  error, not a rounded result. `+` adds two numbers or concatenates two
  strings; no cross-type coercion. Ordering is numeric. Equality compares
  like JSON values by value (numbers by magnitude, object order insignificant);
  unlike types are unequal. `string` on a string returns itself; on numbers
  uses plain decimal without insignificant fractional zeros; on Boolean/null
  uses the lowercase literal. Reject other argument types for `string`.
* Parse errors or constructs outside the grammar fail deployment. Invalid
  operand types at runtime cause `EVALUATION_ERROR` at the originating
  element. Never publish a partial set of writes. Example:
  `= executionCount + 1` at 2 produces the number `3`;
  `="Execution: " + string(executionCount)` produces the string `Execution: 2`.

## DMN: ordered decision tables

Support independent `decision/decisionTable` definitions with
`hitPolicy="FIRST"`, one or more input columns and one named scalar output.
R1 column `typeRef` values: `number`, `string`, `boolean`. Evaluate the input
expressions in column order. A missing/null input can match a wildcard or a
literal null; it does not satisfy a numeric comparison. A non-null input of
the wrong declared type is an evaluation error.

Within a rule, all input entries must match. An empty/whitespace-only entry
or `-` is a wildcard. Accept literal equality tests and unary `<`, `<=`, `>`,
`>=` against a numeric literal. Evaluate rules in source order and return the
first matching output expression. Allow an output column's `outputValues`
to list permitted literals separated by commas; check the selected result
against that list and the output type. No match returns `null`. Reject rule
arity mismatches, other hit policies, decision requirements and boxed
expressions outside the table profile.

The fixture's actual FIRST order is important. An implementation that treats
`stop` as an unconditional exit will fail:

| First applicable condition, **after** increment | Result |
| --- | --- |
| `executionCount > 10` | `break` |
| `command = "terminate"` or `text_command = "terminate"` (two rows) | `break` |
| `executionCount < 3` | `loop` |
| `command = "stop"` or `text_command = "stop"` (two rows) | `break` |
| Otherwise | `loop` |

## Forms: view and submission contract

Support top-level `type: "default"` and ordered `components`. Preserve IDs,
Unicode labels, rows/columns, select label/value order and submit button
labels. Static `text` is literal; `text` starting with `=` must evaluate to a
string at activation. Only bindable input components write variables:

| Component | Resolved view and validation |
| --- | --- |
| `text` | `text` is the evaluated display string. No submit key. |
| `select` | One root key, static distinct string values with labels. Initial `value` is the bound non-null value, otherwise `defaultValue`, otherwise null. Non-null value/default must be a listed value. |
| `textfield` | One root key. Initial `value` is the bound string or null. |
| `button` | Only `action: "submit"`; no variable write. |

Build and save a form view when opening the task: component fields above
replace expression source with display text and include the resolved input
`value`. Other clients need no FEEL interpreter. A bad bound value or display
expression is an incident on entry, with **no** half-open task/timer.
Defaults are display defaults; merely opening or timing out never writes them.
On submit, accept only known input keys with string values and enforce select
membership. Omitted keys leave variables unchanged. The UI submits displayed
non-null inputs; API callers may submit a subset. Invalid fields fail the
operation and leave the same task open. An R2 variable edit affects future
steps, not a previously issued form snapshot.

## Fixture trace

Start `Process_Collect_from_Form` with `{"input":"hello"}`. The start mapping
sets `executionCount` to 0. `Activity_1hidco5` opens `Form_0w2uzn0` displaying
`Caller says: hello` and `Execution: 0`. Either submitting this activation or
firing `Event_14nbybj` reaches `Activity_IncrementCount` exactly once.
`Activity_0modv4y` calls `Decision_0mepvum`; `Gateway_CheckCount` repeats
only for `"loop"` and otherwise takes default `Flow_Complete`.
Continuing every time ends with count **11**, not 10. Sending `"terminate"`
on the first form ends at **1**. These are assertions against the submitted
files, not process/decision IDs to special-case in the engine.
