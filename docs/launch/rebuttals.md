# Thread answers, written before launch day

Written cold, so that at nine in the morning with the adrenaline up the answer is a paste
rather than a defense. Every one concedes something true first, because a rebuttal that
concedes nothing is read as marketing.

## "Semaphore already does this and it is $490."

Semaphore is a good product and it proved the shape: one binary, no Kubernetes, an Ansible
UI people actually like. Our Pro tier is the same $490 for the same reason, single sign-on,
because pricing that above the market would have been us pretending we are further along
than we are. The difference is not the price. It is seven engines instead of one, with a
uniform dry run across all of them, and a signed receipt for every run that verifies
offline with a tool we do not control. If you only run Ansible and do not need the receipt,
Semaphore is the better answer and I would rather you used it.

## "What if Semaphore adds receipts?"

Then someone asked for them, and that is better for me than the alternative, which is that
nobody ever does. A hash chain is not hard to build and I have never said it was. What
neither of us can do is countersign our own customers' history and have it mean anything;
only an outside party can, which is what the Enterprise tier is. Judge the free tiers on
whether the receipt verifies with a tool the vendor does not control.

## "Ascender is AWX but maintained, and it is Apache 2.0."

True, and if a maintained AWX is what you want, take it. It is a real fork by real people.
The difference is footprint: Ascender is AWX, so it is Ansible and the AWX architecture.
This is one binary, seven tools, and an evidence engine in the core. Different bet.

## "But AWX still has commits."

It does. What it has not had is a release in twenty-six months, against a stated goal of one
every three months, and the last released operator fails a fresh install until you patch a
retired container image by hand. Commits on internal tickets are not a release.

## "BSL is not open source."

Correct, and we do not call it that; the word on the page is source-available. Every release
converts to Apache 2.0 two years after it ships, the one reserved right is selling this as a
hosted service, and the verifier that checks our receipts is separately open so that nobody
has to trust us to check our own work.

## "You have no customers."

None. Launched today. Everything above is checkable rather than claimed: the code, the
receipts, the live demo's signed history, and prices published before there is anyone to
charge.
