#!/usr/bin/env perl

use strict;
use warnings;

my %rules = (
    'REVOKE PROCESS ON \*.\* FROM \'tp-\S+-mnt\'' => [],
    'REVOKE ALTER, CREATE, DELETE, DROP, INSERT, SELECT, UPDATE ON `\S+`.`ext_[*%]` FROM \'tp-\S+-mnt\'' => [],
    'REVOKE ALTER, CREATE, DELETE, DROP, INSERT, SELECT, UPDATE ON `\S+`.`dst_[*%]` FROM \'tp-\S+-mnt\'' => [],
    # 'REVOKE ALTER, CREATE, DELETE, DROP, INSERT, SELECT, UPDATE ON `\S+`.`dst_\*` FROM \'tp-\S+-mnt\'' => [],
    # 'GRANT DELETE, INSERT, SELECT, UPDATE ON `\S+`.`(user_payment_details|objects|user_details_schema|plan_template_snapshots|user_details|subscriptions_current|payment_schema|subscriptions_history|subscription_groups|payment_types|operations|payment_minimums|playermap|users|players)` TO \'tp-mrgold-mnt\'' => [],
    'GRANT DELETE, INSERT, SELECT, UPDATE ON `\S+`.`(user_payment_details|objects|user_details_schema|plan_template_snapshots)` TO \'tp-\S+-mnt\'' => [],
    'GRANT DELETE, INSERT, SELECT, UPDATE ON `\S+`.`(user_details|subscriptions_current|payment_schema|subscriptions_history)` TO \'tp-\S+-mnt\'' => [],
    'GRANT DELETE, INSERT, SELECT, UPDATE ON `\S+`.`(subscription_groups|payment_types|operations|payment_minimums)` TO \'tp-\S+-mnt\'' => [],
    'GRANT DELETE, INSERT, SELECT, UPDATE ON `\S+`.`(users|players|playermap)` TO \'tp-\S+-mnt\'' => [],
    'GRANT PROCESS ON \*.\* TO \'tp-\S+-adm\'' => [],
    'GRANT CREATE TEMPORARY TABLES ON `scratch`.* TO \'tp-\S+-ro\'' => [],
    'REVOKE CREATE TEMPORARY TABLES ON `(?!scratch).+`.\* FROM \'tp-\S+-ro\'' => [],
    'GRANT CREATE TEMPORARY TABLES, SELECT ON `scratch`.\* TO \'tp-\S+-ro\'' => [],
    'GRANT ALL PRIVILEGES ON `scratch`.\* TO \'tp-\S+-(adm|mnt)\'' => [],
    'CREATE ROLE \'tp-\S+-(adm|mnt|ro)\'' => [],
    'GRANT SELECT ON `(?!scratch).+`.\* TO \'tp-\S+-ro\'' => [],
    'GRANT USAGE ON \*.\* TO \'tp-\S+-ro\'' => [],
    'GRANT CREATE TEMPORARY TABLES, SELECT ON `(?!scratch).+`\.\* TO \'tp-\S+-mnt\'' => [],
    'GRANT ALTER, CREATE, CREATE TEMPORARY TABLES, DELETE, DROP, INSERT, SELECT, UPDATE ON `(?!zban_).+`.\* TO \'tp-\S+-adm\'' => [],
    'GRANT CREATE TEMPORARY TABLES, DELETE, INSERT, SELECT, UPDATE ON `[Zz][Bb]an_.+`.\* TO \'tp-\S+-adm\'' => [],
    'GRANT PROCESS, USAGE ON \*.\* TO \'tp-\S+-adm\'' => [],
    'GRANT USAGE ON \*.\* TO \'tp-\S+-mnt\'' => [],
);

while (<>) {
    chomp;

    my $found = 0;
    for my $pattern (keys %rules) {
        if (/$pattern/) {
            push @{$rules{$pattern}}, $_;
            $found = 1;
            last;
        }
    }

    $found and next;

    # /REVOKE ALTER, CREATE, DELETE, DROP, INSERT, SELECT, UPDATE ON `flappycasino`.`ext_*` FROM 'tp-flappycasino-mnt'/ and { $count++; next; }
    # /REVOKE ALTER, CREATE, DELETE, DROP, INSERT, SELECT, UPDATE ON `\s+`.`ext_*` FROM 'tp-\s+-mnt'/ and next;
    # /GRANT CREATE TEMPORARY TABLES ON `scratch`.* TO 'tp-\s+-ro'/ and next;

    print "$_\n";
}

print "\nSummary:\n";
for my $pattern (sort keys %rules) {
    my $count = scalar @{$rules{$pattern}};
    print "Pattern: '$pattern' - Count: $count\n";
}
# for my $pattern (sort keys %rules) {
#     my $count = scalar @{$rules{$pattern}};
#     print "Pattern: '$pattern' - Count: $count\n";
#     for my $statement (@{$rules{$pattern}}) {
#         print "  $statement\n";
#     }
# }