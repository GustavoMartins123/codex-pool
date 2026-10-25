package com.gustavomartins.codexpool

import com.gustavomartins.codexpool.data.*
import kotlin.test.*

class CommandSigningTest {
    private val key = ByteArray(CommandSigning.KEY_BYTES) { it.toByte() }
    private val session = "5d1d9c3e-9f1a-4f0e-8a51-2f4b7c1d0e11"
    private val issuedAt = 1761379200000L

    private fun hex(bytes: ByteArray) = bytes.joinToString("") { (it.toInt() and 0xff).toString(16).padStart(2, '0') }

    @Test fun sha256AndHmacMatchPublishedVectors() {
        assertEquals("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", hex(sha256(ByteArray(0))))
        assertEquals("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", hex(sha256("abc".encodeToByteArray())))
        assertEquals("248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
            hex(sha256("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq".encodeToByteArray())))
        // RFC 4231 test cases 1 and 6 (key longer than the block size).
        assertEquals("b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
            hex(hmacSha256(ByteArray(20) { 0x0b }, "Hi There".encodeToByteArray())))
        assertEquals("60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
            hex(hmacSha256(ByteArray(131) { 0xaa.toByte() }, "Test Using Larger Than Block-Size Key - Hash Key First".encodeToByteArray())))
    }

    @Test fun base64UrlRoundTripsAndRejectsForeignInput() {
        for (size in 0..40) {
            val bytes = ByteArray(size) { (it * 37 + 11).toByte() }
            assertContentEquals(bytes, base64UrlDecode(base64UrlEncode(bytes)))
        }
        assertEquals("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", base64UrlEncode(key))
        for (bad in listOf("a", "ab+c", "ab/c", "abc=", "AB")) assertFails { base64UrlDecode(bad) }
        assertFailsWith<IllegalArgumentException> { CommandSigning.decodeKey("AAECAwQ") }
    }

    // Shared with internal/runnerproto/signing_test.go.
    @Test fun signaturesMatchTheRunnerProtocolVectors() {
        assertEquals("CYxfwqA9-ufHeg_CwDnJkw", CommandSigning.keyId(key))
        val turn = CommandSigning.turnInput(session, issuedAt, Agent.Codex, PermissionMode.Gated,
            Workspace(WorkspaceMode.Repo, "https://example.com/r.git"), "olá; 1:2\nrun tests")
        assertEquals("16:codexpool-turn/1;36:$session;13:1761379200000;5:codex;5:gated;4:repo;25:https://example.com/r.git;19:olá; 1:2\nrun tests;",
            turn.decodeToString())
        assertEquals("ahaf6ty62ZJHooWFfwoZqQDLZJfcuxdYTJezViYcY18", CommandSigning.sign(key, turn))
        val approval = CommandSigning.approvalInput(session, "call_7", Decision.Approve, issuedAt, "go test ./...")
        assertEquals("9juZdutr3gbV9kd2RiYJDNxNbAYzZECcxn1OWyeXfx8", CommandSigning.sign(key, approval))
    }

    @Test fun signerLabelsEverySignatureAndNeverReusesAnIssueTime() {
        val encoded = base64UrlEncode(key)
        val other = base64UrlEncode(ByteArray(CommandSigning.KEY_BYTES) { 7 })
        val signer = CommandSigner(keys = { listOf(encoded, other) }, store = {}, now = { issuedAt })
        val target = Session(session, Agent.Claude, "model", Workspace(WorkspaceMode.Ephemeral), PermissionMode.Gated,
            SessionState.WaitingInput, conversationId = session)
        val first = signer.signTurn(target, "list files")
        val second = signer.signTurn(target, "list files")
        assertEquals(issuedAt, first.issuedAt)
        assertEquals(issuedAt + 1, second.issuedAt)
        assertEquals(setOf(CommandSigning.keyId(key), CommandSigning.keyId(CommandSigning.decodeKey(other))), first.signatures.keys)
        assertEquals(CommandSigning.sign(key, CommandSigning.turnInput(session, issuedAt, Agent.Claude, PermissionMode.Gated,
            Workspace(WorkspaceMode.Ephemeral), "list files")), first.signatures[CommandSigning.keyId(key)])
        val approval = Approval("call_1", "go test ./...", "")
        assertNull(signer.signApproval(session, approval, Decision.Deny))
        assertNotNull(signer.signApproval(session, approval, Decision.Approve))
        assertFailsWith<IllegalStateException> { CommandSigner.Disabled.signTurn(target, "list files") }
    }
}
