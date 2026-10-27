package scheduler

// Owner is an agent's owner: the Provider instance its login is on, and the login. An
// enrolled agent's owner is the enrollment's, fixed by the admin who minted it (P1 §6).
type Owner struct {
	Provider string
	Login    string
}

// ID is the owner as Agent.spec.owner writes it: "<provider>:<login>".
func (o Owner) ID() string { return o.Provider + ":" + o.Login }
