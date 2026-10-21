# Routine: codeaf-upgrade

You are running read-only, in routine mode: you write no patch, no branch and
no PR. Your output is proposals only, in the output contract below; a person
(or Claude acting for the owner) promotes them.

## The task

The harness pins CodeAF itself in `.github/codeaf/codeaf-ref`. Decide whether
the factory should take a newer CodeAF.

1. Read `.github/codeaf/codeaf-ref` — it names the pinned commit of the
   upstream CodeAF repository this repo's harness is vendored from.
2. List every upstream commit and every `docs/changes/unreleased/*` entry
   between the pinned commit and the newest `staging-*` tag. Use the
   repository's git history or the upstream hosting site; both are public.
3. Classify the update:
   - `take` — low risk, or it fixes something this factory has actually hit
     (a streaming bug, a hedging miss, a call-log gap, a config key this
     repo sets in `.github/codeaf/codeaf-config.json` or `models.json`).
   - `hold` — risky: name exactly what to test first, and which of this
     harness's behaviours (streaming, hedging, the call log, the config
     keys we set) the change touches.
   - `skip` — the changes between the pin and the newest tag are not worth
     a bump right now; say why in one line.

## Output

- For `take`: one proposal titled `bump .github/codeaf/codeaf-ref to <sha>`,
  with what to verify after the bump in its Done when.
- For `hold`: one proposal describing the risk and the test plan; a person
  decides, you do not.
- For `skip`: no proposal. Say so in your final answer with the reason.

Ground every reason in this repository's own harness — name the file, the
config key or the behaviour it affects. Do not propose workflow changes:
those need a maintainer and are out of your reach.
