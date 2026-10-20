// Command mincost is an ordinary program that asks the user package for the test-only password
// cost. It stands for a production binary reaching for the lower cost, which must die on the panic
// rather than store every new password at the cheapest hash bcrypt has.
package main

import "github.com/kordloom/switchtender/internal/user"

// main asks for the lower cost and never returns normally.
func main() {
	user.UseMinCostForTests()
}
