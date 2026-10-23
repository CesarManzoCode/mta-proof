package classify

// defaultRules is the engine's rule table (contract 3.8). Provider rules
// match the literal starts of contract table 3.2; enhanced rules follow the
// descriptions of RFC 3463 section 3 (and RFC 7505 for null MX); the class
// rule is RFC 3463 section 2's class 4. Subtypes are design choices of the
// contract.
var defaultRules = []Rule{
	// Provider replies, contract table 3.2.
	{ID: "google.rate_ip", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 421, Enhanced: "4.7.28", Text: "Gmail has detected an unusual rate of email originating from your IP address",
		Class: Transient, Subtype: RateLimited},
	{ID: "google.try_later", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 421, Enhanced: "4.7.0", Text: "Try again later, closing connection.",
		Class: Transient, Subtype: General},
	{ID: "google.unauthenticated", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 550, Enhanced: "5.7.26", Text: "This email has been blocked because the sender is unauthenticated.",
		Class: Permanent, Subtype: AuthConfig},
	{ID: "google.dmarc_policy", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 550, Enhanced: "5.7.26", Text: "Unauthenticated email from domain-name is not accepted due to domain's DMARC policy.",
		Placeholder: "domain-name", Class: Permanent, Subtype: AuthConfig},
	{ID: "google.rcpt_rate", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 450, Enhanced: "4.2.1", Text: "The user you are trying to contact is receiving email too quickly.",
		Class: Transient, Subtype: RateLimited},
	{ID: "google.no_account", Tier: TierProvider, Source: "R-GOOGLE-ERR",
		Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist.",
		Class: Permanent, Subtype: NoEmail},
	{ID: "yahoo.tss04", Tier: TierProvider, Source: "R-SUPED-YAHOO",
		Code: 421, Tag: "[TSS04]", Class: Transient, Subtype: General},
	{ID: "yahoo.ipts04", Tier: TierProvider, Source: "R-SUPED-YAHOO",
		Code: 421, Tag: "[IPTS04]", Class: Transient, Subtype: Reputation},
	{ID: "microsoft.auth_level", Tier: TierProvider, Source: "R-MS-515",
		Code: 550, Enhanced: "5.7.515", Text: "Access denied, sending domain <domain> does not meet the required authentication level.",
		Placeholder: "<domain>", Class: Permanent, Subtype: AuthConfig},
	{ID: "microsoft.dmarc_reject", Tier: TierProvider, Source: "R-MS-509",
		Code: 550, Enhanced: "5.7.509", Text: "Access denied, sending domain <domain> does not pass DMARC verification and has a DMARC policy of reject.",
		Placeholder: "<domain>", Class: Permanent, Subtype: AuthConfig},

	// Enhanced status codes.
	{ID: "rfc7505.null_mx", Tier: TierEnhanced, Source: "R-7505", Section: "§4.1",
		Code: 556, Enhanced: "5.1.10", Class: Permanent, Subtype: NullMX},
	{ID: "rfc3463.bad_mailbox", Tier: TierEnhanced, Source: "R-3463", Section: "§3.2",
		Enhanced: "5.1.1", Class: Permanent, Subtype: NoEmail},
	{ID: "rfc3463.mailbox_full", Tier: TierEnhanced, Source: "R-3463", Section: "§3.3",
		Enhanced: "X.2.2", Class: Transient, Subtype: MailboxFull},
	{ID: "rfc3463.mailbox_length", Tier: TierEnhanced, Source: "R-3463", Section: "§3.3",
		Enhanced: "5.2.3", Class: Permanent, Subtype: MessageTooLarge},
	{ID: "rfc3463.too_big", Tier: TierEnhanced, Source: "R-3463", Section: "§3.4",
		Enhanced: "5.3.4", Class: Permanent, Subtype: MessageTooLarge},

	// Generic class rule: only class 4 (D14).
	{ID: "rfc3463.class4", Tier: TierClass, Source: "R-3463", Section: "§2",
		CodeClass: 4, Class: Transient, Subtype: General},
}
