# projects/components: project page presentation

[中文](README.md) | [English](README.en.md)

## Responsibility

Presentation components of the project list and detail pages: the project icon, and the banner shown when provisioning failed. They render the values they are given; nothing here fetches, sends HTTP, or decides whether a project failed — `features/projects/provisioning` owns that decision.

## Files

| File | Purpose |
| --- | --- |
| `project-icon.tsx` | Project icon and colour, falling back to a default icon for an unknown one |
| `provisioning-failure-notice.tsx` | Failure banner: Cloud's reason code, the actionable sentence, and the fact that nothing can be done from here |
| `*.test.tsx` | Component tests that render the component and assert the visible text |

## Dependencies and invariants

Depends on `lucide-react`, `lib/utils` and `features/projects/provisioning` (for the `ProvisioningFailure` type only); used by the list and detail pages in `features/projects` and depended on by nothing lower. New presentation components belong here, and belong in the table above.

The banner renders the reason code beside the sentence because they are different things: a member acts on the sentence, support asks for the exact code. It offers no retry button, because the public retry route refuses a `failed` Operation — a button would promise something the backend rejects. Both values come from upstream; the component never asserts a reason Cloud did not state.

## Testing

The components are pure presentation, so their tests need no MSW and no providers: pass a `ProvisioningFailure`, assert the banner appears, that the sentence and the code are both there, and that no button is. An absent failure is not an input — callers render the banner only when there is one.
