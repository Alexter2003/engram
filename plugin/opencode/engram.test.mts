import assert from "node:assert/strict"
import test from "node:test"

import { shouldNudgeForObservations } from "./engram.ts"

const nowSecs = 1_735_689_600

test("allows the first-save nudge only for a successful empty observation list", () => {
  assert.equal(shouldNudgeForObservations(true, [], nowSecs), true)
  assert.equal(shouldNudgeForObservations(false, [], nowSecs), false)
  assert.equal(shouldNudgeForObservations(true, { observations: [] }, nowSecs), false)
})

test("fails closed for observations without a valid created_at", () => {
  assert.equal(shouldNudgeForObservations(true, [{}], nowSecs), false)
  assert.equal(shouldNudgeForObservations(true, [{ created_at: "not-a-timestamp" }], nowSecs), false)
})

test("preserves the 15-minute observation age threshold", () => {
  const createdAt = (ageSecs: number) => new Date((nowSecs - ageSecs) * 1000).toISOString()

  assert.equal(shouldNudgeForObservations(true, [{ created_at: createdAt(899) }], nowSecs), false)
  assert.equal(shouldNudgeForObservations(true, [{ created_at: createdAt(900) }], nowSecs), true)
  assert.equal(shouldNudgeForObservations(true, [{ created_at: createdAt(901) }], nowSecs), true)
})
