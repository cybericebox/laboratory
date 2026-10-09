# Implementation decisions

Ruling: continue the selected laboratory branch and authorized native method — existing session preference persists — cost if wrong: local task commits share this branch.

Ruling: owner "Да, давай все" authorizes plan plus implementation; no repeated plan handoff — explicit session authorization overrides workflow reapproval defaults — cost if wrong: owner reviews the concrete local commits after execution.

Ruling: complete Tasks1-7 and freeze laboratory before backend Tasks8-9, then frontend Task10 — explicit owner correction — cost if wrong: later consumers wait for the frozen producer contract.

Ruling: no demux UDP/ciphertext analytics; packet/rate probes are engineering validation only — owner correction — cost if wrong: transport analytics remain intentionally absent.

Task 2: Ruling: serialize Delete with off-lock Update resolution and reserve sessions under Table→Conntrack lock order — forced regression reproduced deleted-entry resurrection; old reservations must retire before replacement — cost if wrong: deletes wait up to the bounded 3s lookup.

Task 4: Ruling: separate correctness race suite (-short) from native TLS connection sizing — two instrumented stress runs failed socket acceptance at257/300 and986/1000 while isolated non-race workload passed; preserve failures, do not claim full race stress passed — cost if wrong: instrumented high-concurrency failure remains unresolved pending final controlled validation.

Task 5: Ruling: restore once before namespace admission, retry failed first reads with bounded request context; warm admissions use a separate ready map — prevents smaller counter publication without putting API writes on the request path — cost if wrong: first admission for an unavailable report receives503.

Task 5: Ruling: compact cache owns transformed copies and prewarms all5 required informer types; reader fails on unregistered kinds — readiness now covers every authorization input — cost if wrong: an omitted future input kind fails closed rather than lazily syncing.

Final: Ruling: sourceepochcomments are contract correctness, not polish — clarify ledger cumulativeacrossrestoredboots before consumerimplementation — costifwrong: consumer couldmistakerestoredtotalsforfreshbootdeltas.

Final: Ruling: negotiatedcompressedWSgapclosedbyactualTCP101permessage-deflate fragmented/masked+controlforwardingfixture, counts7compressedbytesbothdirections and payloadunchanged PASS — remainingexoticextensionsmarkPartial — costifwrong: clientsusingunhandledextensionsremainconservative.

Final: Ruling: nativeNode predecessor/finalruntimeequivalence explicitlyverified allregular/bin/sbin/lib/usr/node-agent filesplus/node forbotharches exactSHAidentical(node-runtime-equivalence.json) — nativeproofthereforecoverssameexecutables/deps despiteimage IDs — costifwrong: imageconfigstillcheckedbyfinalmatrix/initialruntimeinspection, no newnativecapacityclaim.

Final: Ruling: Backend/frontendreviewoutofcheckpoint — consumerstagesdeferreduntilfrozenlab — costifwrong: runtimecompatibilitynotreadyuntillaterconsumerchecks.

Final: Ruling: finitefixturesdonotestablishproductioncapacity/throughput — keepcgroup/nativefunctionaldescriptiveresults — costifwrong: resourceplanningcannotinferclusterceilingsfromthese.

Final: Ruling: productionhostmoduleloading/fullKubernetesCRIclusterintegration remainoutsideproof — nativeAWSloadedOVS/geneve; NodekerneltestsusefakeCRIandisolatednamespaces — costifwrong: hostbootstrap/providerintegrationneedsdeploymentchecks, no unconditionalproductionreadinessclaim.

Final: Ruling: priorVPN/GWcorrectness independentlyclosedinpreviousreview; currenttestsverifypublication/consumercompatibility — costifwrong: olderstageevidence notanotherfreshaudit.
